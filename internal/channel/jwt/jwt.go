// Package jwt verifies and signs the RS256 JSON Web Tokens that Bot
// Framework and Google Chat use: just enough of the format for those two,
// with signing keys fetched from the platform and cached.
package jwt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Skew is the clock difference tolerated on exp and nbf.
const Skew = 5 * time.Minute

// Source says what Keys.URL serves.
type Source int

const (
	JWKS    Source = iota // {"keys": [JWK...]}
	OpenID                // an OpenID configuration naming its jwks_uri
	X509Map               // {"kid": "-----BEGIN CERTIFICATE-----..."}
)

// Key is one signing key. Endorsements are Bot Framework's list of channels
// the key may sign for.
type Key struct {
	Public       *rsa.PublicKey
	Endorsements []string
}

// Keys is a cached key set. It refreshes daily, and at most every five
// minutes when a token names a key it does not have (keys rotate).
type Keys struct {
	URL    string
	Source Source
	HTTP   *http.Client

	mu      sync.Mutex
	keys    map[string]Key
	fetched time.Time
	tried   time.Time
}

func (k *Keys) client() *http.Client {
	if k.HTTP != nil {
		return k.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Get returns the key with this id.
func (k *Keys) Get(ctx context.Context, kid string) (Key, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	key, ok := k.keys[kid]
	stale := now.Sub(k.fetched) > 24*time.Hour
	if ok && !stale {
		return key, nil
	}
	if !stale && now.Sub(k.tried) < 5*time.Minute {
		return Key{}, fmt.Errorf("unknown signing key %q", kid)
	}
	k.tried = now
	keys, err := k.fetch(ctx)
	if err != nil {
		if ok {
			return key, nil // keep using a known key while the source is down
		}
		return Key{}, err
	}
	k.keys, k.fetched = keys, now
	if key, ok = keys[kid]; !ok {
		return Key{}, fmt.Errorf("unknown signing key %q", kid)
	}
	return key, nil
}

func (k *Keys) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := k.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signing keys %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

type jwk struct {
	Kid          string   `json:"kid"`
	Kty          string   `json:"kty"`
	N            string   `json:"n"`
	E            string   `json:"e"`
	Endorsements []string `json:"endorsements"`
}

func (k *Keys) fetch(ctx context.Context) (map[string]Key, error) {
	url := k.URL
	if k.Source == OpenID {
		var meta struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := k.getJSON(ctx, url, &meta); err != nil {
			return nil, err
		}
		if meta.JWKSURI == "" {
			return nil, errors.New("openid configuration has no jwks_uri")
		}
		url = meta.JWKSURI
	}
	out := map[string]Key{}
	if k.Source == X509Map {
		var certs map[string]string
		if err := k.getJSON(ctx, url, &certs); err != nil {
			return nil, err
		}
		for kid, p := range certs {
			block, _ := pem.Decode([]byte(p))
			if block == nil {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			if pub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
				out[kid] = Key{Public: pub}
			}
		}
		return out, nil
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := k.getJSON(ctx, url, &set); err != nil {
		return nil, err
	}
	for _, j := range set.Keys {
		if j.Kty != "RSA" {
			continue
		}
		pub, err := rsaKey(j.N, j.E)
		if err != nil {
			continue
		}
		out[j.Kid] = Key{Public: pub, Endorsements: j.Endorsements}
	}
	return out, nil
}

func rsaKey(n, e string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, err
	}
	exp := new(big.Int).SetBytes(eb)
	if !exp.IsInt64() || exp.Int64() > 1<<31 {
		return nil, errors.New("bad exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exp.Int64())}, nil
}

// Claims are a token's payload.
type Claims map[string]any

func (c Claims) String(name string) string {
	s, _ := c[name].(string)
	return s
}

// Audience is aud, which may be one string or a list.
func (c Claims) Audience() []string {
	switch v := c["aud"].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (c Claims) HasAudience(aud string) bool { return slices.Contains(c.Audience(), aud) }

func (c Claims) time(name string) (time.Time, bool) {
	f, ok := c[name].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// Verify checks an RS256 token's signature against the key set and its
// exp and nbf against now. It returns the claims and the signing key; the
// caller checks issuer, audience and anything platform-specific.
func Verify(ctx context.Context, token string, keys *Keys, now time.Time) (Claims, Key, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, Key{}, errors.New("token is not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &hdr); err != nil {
		return nil, Key{}, fmt.Errorf("token header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return nil, Key{}, fmt.Errorf("token algorithm %q is not RS256", hdr.Alg)
	}
	key, err := keys.Get(ctx, hdr.Kid)
	if err != nil {
		return nil, Key{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, Key{}, errors.New("token signature is not base64url")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key.Public, crypto.SHA256, sum[:], sig); err != nil {
		return nil, Key{}, errors.New("token signature does not verify")
	}
	var claims Claims
	if err := decodePart(parts[1], &claims); err != nil {
		return nil, Key{}, fmt.Errorf("token claims: %w", err)
	}
	exp, ok := claims.time("exp")
	if !ok || now.After(exp.Add(Skew)) {
		return nil, Key{}, errors.New("token expired")
	}
	if nbf, ok := claims.time("nbf"); ok && now.Before(nbf.Add(-Skew)) {
		return nil, Key{}, errors.New("token not valid yet")
	}
	return claims, key, nil
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Sign makes an RS256 token.
func Sign(claims map[string]any, key *rsa.PrivateKey, kid string) (string, error) {
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		hdr["kid"] = kid
	}
	h, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	msg := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return msg + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ParsePrivateKey reads a PEM RSA key (PKCS#8 or PKCS#1), as service
// account files hold it.
func ParsePrivateKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("private key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// JWK renders a public key as a JWK, for test key servers.
func JWK(kid string, pub *rsa.PublicKey, endorsements ...string) map[string]any {
	e := big.NewInt(int64(pub.E)).Bytes()
	return map[string]any{
		"kid": kid, "kty": "RSA", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
		"endorsements": endorsements,
	}
}
