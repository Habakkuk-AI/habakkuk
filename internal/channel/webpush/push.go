package webpush

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Kinds of notification. Each device turns each one on or off.
const (
	KindApprovals = "approvals" // a tool call waits for a yes or no
	KindReplies   = "replies"   // a dashboard chat got a reply while no dashboard showed it
	KindSchedules = "schedules" // a scheduled task failed
)

// Kinds lists every kind, in the order the dashboard shows them.
var Kinds = []string{KindApprovals, KindReplies, KindSchedules}

// maxDevices caps the subscriptions kept; the oldest goes first.
const maxDevices = 50

// Subscription is one browser's push subscription, as the dashboard
// registered it.
type Subscription struct {
	ID       string    `json:"id"`
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	Label    string    `json:"label,omitempty"`
	Created  time.Time `json:"created"`
	Off      []string  `json:"off,omitempty"` // kinds this device turned off
}

// Device is a subscription as the dashboard lists it: no keys, no
// endpoint, only which push service it goes through.
type Device struct {
	ID      string          `json:"id"`
	Label   string          `json:"label"`
	Service string          `json:"service"`
	Created time.Time       `json:"created"`
	Kinds   map[string]bool `json:"kinds"`
}

// Message is what a notification shows.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"` // where a tap opens, relative to the dashboard
	Tag   string `json:"tag,omitempty"` // a newer notification with the same tag replaces the older
}

// ErrGone is a subscription the push service no longer knows (the user
// turned notifications off in the browser, or it expired).
var ErrGone = errors.New("the subscription is gone")

// Service keeps the key pair and subscriptions under Dir and sends.
type Service struct {
	Dir string // $HABAKKUK_HOME/push
	// Hosts are the push services a subscription may name: exact hosts,
	// or "*.example.com" for any host under it. Nil is DefaultHosts.
	Hosts []string
	// Contact is the VAPID subject: a mailto: address or an https URL push
	// services can reach the sender at.
	Contact string
	Client  *http.Client // nil: a client that only reaches public addresses
	Log     *slog.Logger
	Now     func() time.Time

	mu   sync.Mutex // the subscriptions file
	keys *Keys
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	c := tools.GuardedClient(30 * time.Second)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("push services do not redirect") }
	return c
}

// Keys returns the VAPID key pair, creating it on first use.
func (s *Service) Keys() (*Keys, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		k, err := LoadOrCreateKeys(filepath.Join(s.Dir, "vapid.json"))
		if err != nil {
			return nil, err
		}
		s.keys = k
	}
	return s.keys, nil
}

func (s *Service) file() string { return filepath.Join(s.Dir, "subscriptions.json") }

func (s *Service) load() ([]Subscription, error) {
	data, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var subs []Subscription
	if err := json.Unmarshal(data, &subs); err != nil {
		return nil, fmt.Errorf("%s: %w", s.file(), err)
	}
	return subs, nil
}

func (s *Service) save(subs []Subscription) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(s.file(), append(data, '\n'), 0o600)
}

// SubscriptionID is a subscription's id: a hash of its endpoint, so the same
// browser registering again keeps its id and settings.
func SubscriptionID(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return "dev_" + hex.EncodeToString(sum[:8])
}

// DefaultHosts are the browsers' own push services: Chrome and other
// Chromium browsers (FCM), Firefox (Mozilla), Safari (Apple) and Edge
// (Microsoft's WNS). Only these may receive messages, so someone who got hold of the
// dashboard token cannot register a server of their own to read approval
// and reply notices from then on.
var DefaultHosts = []string{"fcm.googleapis.com", "*.push.services.mozilla.com", "web.push.apple.com", "*.notify.windows.com"}

func (s *Service) hosts() []string {
	if s.Hosts != nil {
		return s.Hosts
	}
	return DefaultHosts
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allowed {
		if suffix, ok := strings.CutPrefix(a, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == a {
			return true
		}
	}
	return false
}

// checkEndpoint accepts only an https URL on a known push service: the
// server will POST notices to it.
func (s *Service) checkEndpoint(endpoint string) error {
	if len(endpoint) > 2048 {
		return errors.New("the endpoint is too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Port() != "" {
		return errors.New("the endpoint must be an https URL")
	}
	if !hostAllowed(u.Hostname(), s.hosts()) {
		return fmt.Errorf("%s is not a browser push service this server sends to (%s)", u.Hostname(), strings.Join(s.hosts(), ", "))
	}
	if err := tools.CheckURL(endpoint); err != nil {
		return fmt.Errorf("the endpoint: %w", err)
	}
	return nil
}

// Subscribe adds a browser's subscription, or updates the one with the same
// endpoint (keeping its settings). off lists kinds to turn off for a new
// one. It returns the subscription's id.
func (s *Service) Subscribe(endpoint, p256dh, auth, label string, off []string) (string, error) {
	if err := s.checkEndpoint(endpoint); err != nil {
		return "", err
	}
	pub, err := decodeB64(p256dh)
	if err != nil || len(pub) != 65 || pub[0] != 4 {
		return "", errors.New("keys.p256dh must be an uncompressed P-256 public key")
	}
	if a, err := decodeB64(auth); err != nil || len(a) != 16 {
		return "", errors.New("keys.auth must be 16 bytes")
	}
	label = strings.TrimSpace(label)
	if len([]rune(label)) > 80 {
		label = string([]rune(label)[:80])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.load()
	if err != nil {
		return "", err
	}
	id := SubscriptionID(endpoint)
	sub := Subscription{ID: id, Endpoint: endpoint, P256dh: p256dh, Auth: auth, Label: label, Created: s.now().UTC(), Off: knownKinds(off)}
	if i := slices.IndexFunc(subs, func(x Subscription) bool { return x.ID == id }); i >= 0 {
		sub.Created, sub.Off = subs[i].Created, subs[i].Off
		if label == "" {
			sub.Label = subs[i].Label
		}
		subs[i] = sub
	} else {
		subs = append(subs, sub)
		if len(subs) > maxDevices {
			subs = subs[len(subs)-maxDevices:]
		}
	}
	return id, s.save(subs)
}

func knownKinds(kinds []string) []string {
	var out []string
	for _, k := range Kinds {
		if slices.Contains(kinds, k) {
			out = append(out, k)
		}
	}
	return out
}

// SetKinds turns kinds on or off for one device; kinds not named keep
// their setting.
func (s *Service) SetKinds(id string, on map[string]bool) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.load()
	if err != nil {
		return Device{}, err
	}
	i := slices.IndexFunc(subs, func(x Subscription) bool { return x.ID == id })
	if i < 0 {
		return Device{}, os.ErrNotExist
	}
	off := subs[i].Off
	for k, v := range on {
		if !slices.Contains(Kinds, k) {
			return Device{}, fmt.Errorf("unknown kind %q (want %s)", k, strings.Join(Kinds, ", "))
		}
		off = slices.DeleteFunc(off, func(x string) bool { return x == k })
		if !v {
			off = append(off, k)
		}
	}
	subs[i].Off = knownKinds(off)
	return device(subs[i]), s.save(subs)
}

// Remove forgets a device. Removing one that is not there is not an error.
func (s *Service) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.load()
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(subs, func(x Subscription) bool { return x.ID == id })
	return s.save(kept)
}

// Devices lists the registered devices, oldest first.
func (s *Service) Devices() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(subs))
	for _, sub := range subs {
		out = append(out, device(sub))
	}
	return out, nil
}

func device(sub Subscription) Device {
	d := Device{ID: sub.ID, Label: sub.Label, Created: sub.Created, Kinds: map[string]bool{}}
	if u, err := url.Parse(sub.Endpoint); err == nil {
		d.Service = u.Hostname()
	}
	for _, k := range Kinds {
		d.Kinds[k] = !slices.Contains(sub.Off, k)
	}
	return d
}

// Notify sends m to every device that has kind on, and forgets devices
// whose push service says they are gone. It returns how many it reached.
func (s *Service) Notify(ctx context.Context, kind string, m Message) int {
	s.mu.Lock()
	subs, err := s.load()
	s.mu.Unlock()
	if err != nil {
		s.warn("read push subscriptions", err)
		return 0
	}
	var targets []Subscription
	for _, sub := range subs {
		if !slices.Contains(sub.Off, kind) {
			targets = append(targets, sub)
		}
	}
	return s.sendAll(ctx, targets, kind, m)
}

// Test sends m to one device, whatever its settings.
func (s *Service) Test(ctx context.Context, id string, m Message) error {
	s.mu.Lock()
	subs, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(subs, func(x Subscription) bool { return x.ID == id })
	if i < 0 {
		return os.ErrNotExist
	}
	err = s.send(ctx, subs[i], "", m)
	if errors.Is(err, ErrGone) {
		_ = s.Remove(id)
	}
	return err
}

func (s *Service) sendAll(ctx context.Context, subs []Subscription, kind string, m Message) int {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ok   int
		gone []string
	)
	for _, sub := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.send(ctx, sub, kind, m)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrGone):
				gone = append(gone, sub.ID)
			default:
				s.warn("web push to "+device(sub).Service, err)
			}
		}()
	}
	wg.Wait()
	for _, id := range gone {
		if err := s.Remove(id); err != nil {
			s.warn("forget a gone push subscription", err)
		}
	}
	return ok
}

// send encrypts and posts one message.
func (s *Service) send(ctx context.Context, sub Subscription, kind string, m Message) error {
	keys, err := s.Keys()
	if err != nil {
		return err
	}
	pub, err := decodeB64(sub.P256dh)
	if err != nil {
		return err
	}
	auth, err := decodeB64(sub.Auth)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	body, err := encrypt(payload, pub, auth)
	if err != nil {
		return err
	}
	// The file the subscriptions are in is not trusted to hold only what
	// Subscribe accepted: check the endpoint again before posting to it.
	if err := s.checkEndpoint(sub.Endpoint); err != nil {
		return err
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil {
		return err
	}
	vapid, err := keys.authorization(u.Scheme+"://"+u.Host, s.contact(), s.now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", vapid)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400") // a phone that is off for a day still gets it
	urgency := "normal"
	if kind == KindApprovals {
		urgency = "high"
	}
	req.Header.Set("Urgency", urgency)
	res, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return ErrGone
	}
	return fmt.Errorf("push service answered %s: %s", res.Status, strings.TrimSpace(string(detail)))
}

// tokenFile records which dashboard token the devices were registered
// under, as a salted hash.
type tokenFile struct {
	Salt string `json:"salt"`
	Sum  string `json:"sum"`
}

// CheckToken forgets every device when the dashboard token is not the one
// they were registered under: whoever registered a device may have had a
// token that was since rotated. It returns how many devices it forgot. The
// first call only records the token.
func (s *Service) CheckToken(token string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.Dir, "token.json")
	var tf tokenFile
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return 0, err
	default:
		if err := json.Unmarshal(data, &tf); err != nil {
			tf = tokenFile{}
		}
	}
	sum := func(salt string) string {
		mac := hmac.New(sha256.New, []byte(salt))
		mac.Write([]byte(token))
		return hex.EncodeToString(mac.Sum(nil))
	}
	if tf.Salt != "" && hmac.Equal([]byte(sum(tf.Salt)), []byte(tf.Sum)) {
		return 0, nil // same token
	}
	forgot := 0
	if tf.Salt != "" { // the token changed: devices registered before go
		subs, err := s.load()
		if err != nil {
			return 0, err
		}
		forgot = len(subs)
		if forgot > 0 {
			if err := s.save(nil); err != nil {
				return 0, err
			}
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return 0, err
	}
	tf.Salt = hex.EncodeToString(salt)
	tf.Sum = sum(tf.Salt)
	out, _ := json.Marshal(tf)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return 0, err
	}
	return forgot, store.WriteFileAtomic(path, out, 0o600)
}

// DefaultContact is the VAPID subject when none is configured.
const DefaultContact = "https://habakkuk.ai"

func (s *Service) contact() string {
	if s.Contact != "" {
		return s.Contact
	}
	return DefaultContact
}

func (s *Service) warn(what string, err error) {
	if s.Log != nil {
		s.Log.Warn(what+" failed", "err", err)
	}
}
