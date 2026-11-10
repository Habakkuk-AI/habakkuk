package webpush

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The worked example in RFC 8291 section 5.
func TestEncryptMatchesRFC8291(t *testing.T) {
	curve := ecdh.P256()
	asPriv, err := curve.NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPub, err := curve.NewPublicKey(mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b64.EncodeToString(asPriv.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("server public key %s", got)
	}
	body, err := seal([]byte("When I grow up, I want to be a watermelon"), uaPub, mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), asPriv,
		mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := b64.EncodeToString(body); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
}

func TestKeysPersistAndSignVAPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push", "vapid.json")
	k1, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %o", st.Mode().Perm())
	}
	k2, err := LoadOrCreateKeys(path)
	if err != nil || k2.Public != k1.Public {
		t.Fatalf("key changed on reload: %v", err)
	}
	if len(mustB64(t, k1.Public)) != 65 {
		t.Fatal("public key is not an uncompressed P-256 point")
	}

	auth, err := k1.authorization("https://push.example.com", "mailto:sam@example.com", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(auth, "vapid t=")
	if !ok {
		t.Fatal(auth)
	}
	jwt, key, _ := strings.Cut(rest, ", k=")
	if key != k1.Public {
		t.Fatal("k= is not the public key")
	}
	parts := strings.Split(jwt, ".")
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	json.Unmarshal(mustB64(t, parts[1]), &claims)
	if claims.Aud != "https://push.example.com" || claims.Sub != "mailto:sam@example.com" || claims.Exp != 1_800_000_000+12*3600 {
		t.Fatalf("claims %+v", claims)
	}
	pub := mustB64(t, k1.Public)
	x, y := new(big.Int).SetBytes(pub[1:33]), new(big.Int).SetBytes(pub[33:])
	sig := mustB64(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("JWT signature does not verify")
	}
}

// browser is a fake subscriber: its keys, and what its push service got.
type browser struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) browser {
	priv, err := ecdh.P256().GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return browser{priv: priv, auth: []byte("0123456789abcdef")}
}

// open decrypts a body the way a browser does.
func (b browser) open(t *testing.T, body []byte) Message {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	asPub := body[21 : 21+idlen]
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := b.priv.ECDH(pub)
	cek, nonce, err := contentKeys(shared, b.auth, b.priv.PublicKey().Bytes(), asPub, salt)
	if err != nil {
		t.Fatal(err)
	}
	plain := gcmOpen(t, cek, nonce, body[21+idlen:])
	var m Message
	if err := json.Unmarshal(plain[:len(plain)-1], &m); err != nil {
		t.Fatalf("%v: %q", err, plain)
	}
	return m
}

func TestSubscribeNotifyAndForgetGone(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]*http.Request{}
	bodies := map[string][][]byte{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = append(got[r.URL.Path], r)
		bodies[r.URL.Path] = append(bodies[r.URL.Path], data)
		mu.Unlock()
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	s := &Service{Dir: t.TempDir(), Client: srv.Client(), Contact: "mailto:sam@example.com"}
	phone, laptop := newBrowser(t), newBrowser(t)
	// The test server is on loopback, which Subscribe refuses; register
	// through a public-looking name, then point the stored endpoint at it.
	idPhone, err := s.Subscribe("https://push.example.com/phone", b64.EncodeToString(phone.priv.PublicKey().Bytes()), b64.EncodeToString(phone.auth), "Phone", nil)
	if err != nil {
		t.Fatal(err)
	}
	idLaptop, err := s.Subscribe("https://push.example.com/gone", b64.EncodeToString(laptop.priv.PublicKey().Bytes()), b64.EncodeToString(laptop.auth), "Laptop", []string{KindReplies})
	if err != nil {
		t.Fatal(err)
	}
	subs, _ := s.load()
	for i := range subs {
		subs[i].Endpoint = strings.Replace(subs[i].Endpoint, "https://push.example.com", srv.URL, 1)
	}
	s.save(subs)

	// Replies are off on the laptop: only the phone gets one.
	if n := s.Notify(context.Background(), KindReplies, Message{Title: "Habakkuk", Body: "Your report is ready"}); n != 1 {
		t.Fatalf("reached %d", n)
	}
	if m := phone.open(t, bodies["/phone"][0]); m.Body != "Your report is ready" {
		t.Fatalf("phone got %+v", m)
	}
	r := got["/phone"][0]
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
		t.Fatalf("headers %v", r.Header)
	}

	// An approval goes to both; the laptop's push service says it is gone,
	// so the laptop is forgotten.
	if n := s.Notify(context.Background(), KindApprovals, Message{Title: "Approval needed"}); n != 1 {
		t.Fatalf("reached %d", n)
	}
	if got["/phone"][1].Header.Get("Urgency") != "high" {
		t.Fatal("approvals should be urgent")
	}
	devs, _ := s.Devices()
	if len(devs) != 1 || devs[0].ID != idPhone || devs[0].Label != "Phone" || devs[0].Kinds[KindApprovals] != true {
		t.Fatalf("devices %+v (laptop %s should be gone)", devs, idLaptop)
	}

	// Settings change per kind and survive a new registration.
	if _, err := s.SetKinds(idPhone, map[string]bool{KindSchedules: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetKinds(idPhone, map[string]bool{"nonsense": true}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	devs, _ = s.Devices()
	if devs[0].Kinds[KindSchedules] || !devs[0].Kinds[KindReplies] {
		t.Fatalf("kinds %+v", devs[0].Kinds)
	}
	if n := s.Notify(context.Background(), KindSchedules, Message{Title: "x"}); n != 0 {
		t.Fatal("sent a kind the device turned off")
	}
	if err := s.Remove(idPhone); err != nil {
		t.Fatal(err)
	}
	if devs, _ := s.Devices(); len(devs) != 0 {
		t.Fatal("not removed")
	}
}

func TestSubscribeRefusesBadInput(t *testing.T) {
	s := &Service{Dir: t.TempDir()}
	b := newBrowser(t)
	key, auth := b64.EncodeToString(b.priv.PublicKey().Bytes()), b64.EncodeToString(b.auth)
	for _, c := range []struct{ endpoint, key, auth string }{
		{"http://push.example.com/x", key, auth},     // not https
		{"https://127.0.0.1/x", key, auth},           // loopback
		{"https://10.0.0.8/x", key, auth},            // private
		{"https://push.example.com/x", "AAAA", auth}, // bad key
		{"https://push.example.com/x", key, "AAAA"},  // bad auth
	} {
		if _, err := s.Subscribe(c.endpoint, c.key, c.auth, "", nil); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func gcmOpen(t *testing.T, key, nonce, sealed []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}
