package jwt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyJWKSAndOpenID(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	var hits atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/openid", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"jwks_uri": srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{JWK("k1", &key.PublicKey, "msteams")}})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	keys := &Keys{URL: srv.URL + "/openid", Source: OpenID}
	now := time.Now()

	tok, _ := Sign(map[string]any{"iss": "me", "aud": []string{"a", "b"}, "exp": now.Add(time.Hour).Unix()}, key, "k1")
	claims, k, err := Verify(context.Background(), tok, keys, now)
	if err != nil {
		t.Fatal(err)
	}
	if claims.String("iss") != "me" || !claims.HasAudience("b") || len(k.Endorsements) != 1 {
		t.Fatalf("claims %v key %+v", claims, k)
	}

	expired, _ := Sign(map[string]any{"exp": now.Add(-time.Hour).Unix()}, key, "k1")
	if _, _, err := Verify(context.Background(), expired, keys, now); err == nil {
		t.Fatal("expired token verified")
	}
	forged, _ := Sign(map[string]any{"exp": now.Add(time.Hour).Unix()}, other, "k1")
	if _, _, err := Verify(context.Background(), forged, keys, now); err == nil {
		t.Fatal("token signed by another key verified")
	}
	parts := strings.Split(tok, ".")
	if _, _, err := Verify(context.Background(), `eyJhbGciOiJub25lIiwia2lkIjoiazEifQ.`+parts[1]+".", keys, now); err == nil {
		t.Fatal("alg none verified")
	}
	unknown, _ := Sign(map[string]any{"exp": now.Add(time.Hour).Unix()}, key, "k9")
	for range 3 {
		if _, _, err := Verify(context.Background(), unknown, keys, now); err == nil {
			t.Fatal("unknown key id verified")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("fetched keys %d times; keys fetched a moment ago are not fetched again for an unknown id", n)
	}
}

func TestVerifyX509Map(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chat"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"c1": certPEM})
	}))
	defer srv.Close()
	tok, _ := Sign(map[string]any{"aud": "123", "exp": time.Now().Add(time.Hour).Unix()}, key, "c1")
	claims, _, err := Verify(context.Background(), tok, &Keys{URL: srv.URL, Source: X509Map}, time.Now())
	if err != nil || !claims.HasAudience("123") {
		t.Fatalf("claims %v err %v", claims, err)
	}
}

func TestParsePrivateKey(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	got, err := ParsePrivateKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if err != nil || !got.Equal(key) {
		t.Fatalf("pkcs8: %v", err)
	}
	got, err = ParsePrivateKey(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	if err != nil || !got.Equal(key) {
		t.Fatalf("pkcs1: %v", err)
	}
}
