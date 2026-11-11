// Package webpush sends Web Push notifications (RFC 8030) to browsers and
// installed dashboards, with no service in between but the browser's own
// push service. It holds the server's VAPID key pair (RFC 8292), encrypts
// each message for one subscription (RFC 8291, aes128gcm from RFC 8188), and
// keeps the subscriptions the dashboard registered. Only the standard
// library is used.
package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

var b64 = base64.RawURLEncoding

// decodeB64 reads base64url with or without padding, as browsers send it.
func decodeB64(s string) ([]byte, error) {
	if b, err := b64.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// Keys is the server's VAPID key pair. Browsers tie a subscription to the
// public key, so it must not change: it is created once and kept in the
// Habakkuk home.
type Keys struct {
	priv *ecdsa.PrivateKey
	// Public is the uncompressed P-256 public key, base64url, as
	// PushManager.subscribe wants it for applicationServerKey.
	Public string
}

type keyFile struct {
	Private string `json:"private"` // the raw 32-byte scalar, base64url
	Public  string `json:"public"`
}

// LoadOrCreateKeys reads the key pair from path, creating it (owner-only)
// when it does not exist.
func LoadOrCreateKeys(path string) (*Keys, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		k, err := keysFrom(priv)
		if err != nil {
			return nil, err
		}
		raw, err := priv.Bytes()
		if err != nil {
			return nil, err
		}
		out, _ := json.MarshalIndent(keyFile{Private: b64.EncodeToString(raw), Public: k.Public}, "", "  ")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := store.WriteFileAtomic(path, append(out, '\n'), 0o600); err != nil {
			return nil, err
		}
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := decodeB64(kf.Private)
	if err != nil {
		return nil, fmt.Errorf("%s: private key: %w", path, err)
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("%s: private key: %w", path, err)
	}
	return keysFrom(priv)
}

func keysFrom(priv *ecdsa.PrivateKey) (*Keys, error) {
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &Keys{priv: priv, Public: b64.EncodeToString(pub)}, nil
}

// authorization is the VAPID Authorization header for a push service:
// a JWT for the service's origin, signed with ES256, and the public key.
func (k *Keys) authorization(audience, subject string, now time.Time) (string, error) {
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": audience, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	if err != nil {
		return "", err
	}
	signing := header + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS wants r and s as two fixed 32-byte halves
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + k.Public, nil
}

// recordSize is the aes128gcm record size: one record holds any payload
// this package sends.
const recordSize = 4096

// maxPayload keeps a message in one record, under the 4 KB push services
// accept.
const maxPayload = 3800

// encrypt seals plaintext for a subscription (RFC 8291): an ephemeral ECDH
// key agreed with the browser's p256dh key, mixed with its auth secret,
// gives the content key and nonce; the body is the aes128gcm header (salt,
// record size, the ephemeral public key) and one record.
func encrypt(plaintext, p256dh, auth []byte) ([]byte, error) {
	if len(plaintext) > maxPayload {
		return nil, fmt.Errorf("push message is %d bytes; the limit is %d", len(plaintext), maxPayload)
	}
	curve := ecdh.P256()
	uaPub, err := curve.NewPublicKey(p256dh)
	if err != nil {
		return nil, fmt.Errorf("subscription key: %w", err)
	}
	if len(auth) != 16 {
		return nil, fmt.Errorf("subscription auth secret is %d bytes, want 16", len(auth))
	}
	asPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return seal(plaintext, uaPub, auth, asPriv, salt)
}

// seal is encrypt with the ephemeral key and salt given, for tests.
func seal(plaintext []byte, uaPub *ecdh.PublicKey, auth []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()
	cek, nonce, err := contentKeys(shared, auth, uaPub.Bytes(), asPub, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// One record, the last: the payload followed by the 0x02 delimiter.
	record := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil)
	out := make([]byte, 0, 16+4+1+len(asPub)+len(record))
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return append(out, record...), nil
}

// contentKeys derives the content encryption key and nonce (RFC 8291
// section 3.4 and RFC 8188 section 2.2).
func contentKeys(shared, auth, uaPub, asPub, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, shared, auth)
	if err != nil {
		return nil, nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	if nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12); err != nil {
		return nil, nil, err
	}
	return cek, nonce, nil
}
