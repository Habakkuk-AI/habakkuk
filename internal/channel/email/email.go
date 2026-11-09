// Package email is the email adapter: it reads a mailbox over IMAP (IDLE
// when the server has it, else polling) and answers over SMTP. Each thread
// is a conversation; replies carry the threading headers and a "Re:"
// subject so mail clients keep them together.
package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
)

// idleFor bounds one IDLE: the mailbox is searched again after it even when
// the server announced nothing, so a missed announcement costs minutes, not
// a day. A variable so tests can shorten it.
var idleFor = 5 * time.Minute

const (
	maxMessage  = 40 << 20 // larger messages arrive as their text only
	maxSendFile = 18 << 20 // base64 grows it by a third; most servers take 25 MB
	maxThreads  = 2000     // reply details kept for this many threads
	maxRefs     = 20       // References kept on a reply: the root and the newest
)

// Config is the adapter's settings, secrets resolved.
type Config struct {
	Address              string
	Name                 string
	IMAP, SMTP           string // host:port
	IMAPTLS, SMTPTLS     string // auto | tls | starttls | none
	Username, Password   string
	Folder               string
	Poll                 time.Duration
	AllowUnauthenticated bool
	StatePath            string // JSON file under $HABAKKUK_HOME/channels
}

type Adapter struct {
	cfg Config
	Log *slog.Logger
	TLS *tls.Config // nil: system roots

	mu    sync.Mutex
	st    state
	saved bool // st was loaded from disk
}

// state is what the adapter remembers between runs: where it stopped in the
// mailbox, and how to reply in each thread.
type state struct {
	UIDValidity uint32             `json:"uid_validity"`
	LastUID     uint32             `json:"last_uid"`
	Threads     map[string]*thread `json:"threads"`
}

type thread struct {
	Subject string    `json:"subject"`
	Refs    []string  `json:"refs"` // References for the next reply, newest last
	Seen    time.Time `json:"seen"`
}

func New(cfg Config, log *slog.Logger) *Adapter {
	if cfg.Folder == "" {
		cfg.Folder = "INBOX"
	}
	if cfg.Poll == 0 {
		cfg.Poll = time.Minute
	}
	if cfg.Username == "" {
		cfg.Username = cfg.Address
	}
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{cfg: cfg, Log: log}
}

func (a *Adapter) MaxLen() int             { return 100_000 }
func (a *Adapter) Format(md string) string { return md } // Markdown reads fine as plain text
func (a *Adapter) MaxFileSize() int64      { return maxSendFile }
func (a *Adapter) StaleAfter() time.Duration {
	return 3*max(a.cfg.Poll, idleFor) + time.Minute
}

func (a *Adapter) tlsConfig(hostport string) *tls.Config {
	host, _, _ := net.SplitHostPort(hostport)
	c := &tls.Config{}
	if a.TLS != nil {
		c = a.TLS.Clone()
	}
	if c.ServerName == "" {
		c.ServerName = host
	}
	return c
}

// mode resolves "auto": implicit TLS on the well-known TLS port, else
// STARTTLS.
func mode(m, hostport, tlsPort string) string {
	if m != "auto" && m != "" {
		return m
	}
	if _, port, _ := net.SplitHostPort(hostport); port == tlsPort {
		return "tls"
	}
	return "starttls"
}

func (a *Adapter) loadState() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.saved {
		return nil
	}
	if a.cfg.StatePath != "" {
		if err := channel.LoadState(a.cfg.StatePath, &a.st); err != nil {
			return fmt.Errorf("email state: %w", err)
		}
	}
	if a.st.Threads == nil {
		a.st.Threads = map[string]*thread{}
	}
	a.saved = true
	return nil
}

// saveLocked writes the state; the caller holds mu.
func (a *Adapter) saveLocked() {
	if a.cfg.StatePath == "" {
		return
	}
	if len(a.st.Threads) > maxThreads {
		keys := make([]string, 0, len(a.st.Threads))
		for k := range a.st.Threads {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(x, y string) int { return a.st.Threads[x].Seen.Compare(a.st.Threads[y].Seen) })
		for _, k := range keys[:len(keys)-maxThreads] {
			delete(a.st.Threads, k)
		}
	}
	if err := channel.SaveState(a.cfg.StatePath, &a.st); err != nil {
		a.Log.Warn("save email state failed", "err", err)
	}
}

func (a *Adapter) dialIMAP(newMail chan<- struct{}) (*imapclient.Client, error) {
	opts := &imapclient.Options{
		TLSConfig: a.tlsConfig(a.cfg.IMAP),
		Dialer:    &net.Dialer{Timeout: 30 * time.Second, KeepAlive: time.Minute},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					select {
					case newMail <- struct{}{}:
					default:
					}
				}
			},
		},
	}
	switch mode(a.cfg.IMAPTLS, a.cfg.IMAP, "993") {
	case "tls":
		return imapclient.DialTLS(a.cfg.IMAP, opts)
	case "starttls":
		return imapclient.DialStartTLS(a.cfg.IMAP, opts)
	default:
		return imapclient.DialInsecure(a.cfg.IMAP, opts)
	}
}

// Run watches the folder and delivers new mail. On the first run it starts
// at the newest message, so old mail is never answered.
func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	if err := a.loadState(); err != nil {
		return channel.Fatal{Err: err}
	}
	newMail := make(chan struct{}, 1)
	c, err := a.dialIMAP(newMail)
	if err != nil {
		return fmt.Errorf("imap %s: %w", a.cfg.IMAP, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() { _ = c.Close() }()
	if err := c.Login(a.cfg.Username, a.cfg.Password).Wait(); err != nil {
		var ierr *imap.Error
		if errors.As(err, &ierr) && ierr.Type == imap.StatusResponseTypeNo {
			return channel.Fatal{Err: fmt.Errorf("imap login as %s: %w", a.cfg.Username, err)}
		}
		return fmt.Errorf("imap login: %w", err)
	}
	sel, err := c.Select(a.cfg.Folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return channel.Fatal{Err: fmt.Errorf("imap select %s: %w", a.cfg.Folder, err)}
	}
	if err := a.start(c, sel); err != nil {
		return err
	}
	canIdle := c.Caps().Has(imap.CapIdle) || c.Caps().Has(imap.CapIMAP4rev2)
	in.Connected(a.cfg.Address)
	for ctx.Err() == nil {
		if err := a.fetchNew(ctx, c, in); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		in.Beat()
		if err := a.wait(ctx, c, canIdle, newMail); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return nil
}

// start picks up where the last run stopped. A new mailbox, or one whose
// UIDVALIDITY changed (rebuilt by the server), starts at its newest message.
func (a *Adapter) start(c *imapclient.Client, sel *imap.SelectData) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.UIDValidity == sel.UIDValidity && a.st.UIDValidity != 0 {
		return nil
	}
	last := uint32(0)
	if sel.UIDNext > 0 {
		last = uint32(sel.UIDNext) - 1
	} else if sel.NumMessages > 0 {
		data, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return fmt.Errorf("imap search: %w", err)
		}
		for _, uid := range data.AllUIDs() {
			last = max(last, uint32(uid))
		}
	}
	a.st.UIDValidity, a.st.LastUID = sel.UIDValidity, last
	a.saveLocked()
	return nil
}

// wait returns when new mail may have arrived: IDLE until the server
// announces a message (or idleFor passes), or else one poll interval.
func (a *Adapter) wait(ctx context.Context, c *imapclient.Client, canIdle bool, newMail <-chan struct{}) error {
	if !canIdle {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.cfg.Poll):
		}
		return c.Noop().Wait()
	}
	idle, err := c.Idle()
	if err != nil {
		return fmt.Errorf("imap idle: %w", err)
	}
	t := time.NewTimer(idleFor)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-newMail:
	case <-t.C:
	}
	if err := idle.Close(); err != nil {
		return err
	}
	return idle.Wait()
}

// fetchNew delivers every message after the last one seen, oldest first,
// and records each as seen once delivered.
func (a *Adapter) fetchNew(ctx context.Context, c *imapclient.Client, in channel.Inbox) error {
	a.mu.Lock()
	last := a.st.LastUID
	a.mu.Unlock()
	data, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{{{Start: imap.UID(last + 1), Stop: 0}}}}, nil).Wait()
	if err != nil {
		return fmt.Errorf("imap search: %w", err)
	}
	var uids []imap.UID
	for _, uid := range data.AllUIDs() {
		if uint32(uid) > last { // "N:*" always includes the newest message
			uids = append(uids, uid)
		}
	}
	slices.Sort(uids)
	for _, uid := range uids {
		if ctx.Err() != nil {
			return nil
		}
		raw, err := a.fetch(c, uid)
		if err != nil {
			return err
		}
		if raw != nil {
			a.handle(ctx, in, uint32(uid), raw)
		}
		a.mu.Lock()
		a.st.LastUID = uint32(uid)
		a.saveLocked()
		a.mu.Unlock()
	}
	return nil
}

// fetch reads one message without marking it read. A message over
// maxMessage comes back as its header with a note for a body.
func (a *Adapter) fetch(c *imapclient.Client, uid imap.UID) ([]byte, error) {
	msgs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}
	if len(msgs) == 0 {
		return nil, nil // expunged since the search
	}
	section := &imap.FetchItemBodySection{Peek: true}
	big := msgs[0].RFC822Size > maxMessage
	if big {
		section.Specifier = imap.PartSpecifierHeader
	}
	msgs, err = c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	raw := msgs[0].FindBodySection(section)
	if big {
		hdr := bytes.TrimRight(raw, "\r\n")
		raw = append(hdr, fmt.Sprintf("\r\n\r\n(This message is %d MB, too large to read here; only its header arrived.)\r\n", msgs[0].RFC822Size>>20)...)
	}
	return raw, nil
}

func (a *Adapter) handle(ctx context.Context, in channel.Inbox, uid uint32, raw []byte) {
	m, err := parse(raw)
	if err != nil {
		a.Log.Warn("unreadable email skipped", "uid", uid, "err", err)
		return
	}
	log := a.Log.With("from", m.From, "message_id", m.ID)
	switch {
	case m.From == "":
		log.Debug("email without a From address skipped")
		return
	case strings.EqualFold(m.From, a.cfg.Address):
		return // our own reply, filed or copied into this folder
	case m.Automated != "":
		log.Debug("automatic email skipped", "why", m.Automated)
		return
	case !m.Authentic && !a.cfg.AllowUnauthenticated:
		log.Warn("email skipped: no passing DMARC or aligned DKIM result from the receiving server (set allow_unauthenticated to accept it)")
		return
	}
	if m.ID == "" {
		a.mu.Lock()
		m.ID = fmt.Sprintf("%d.%d@habakkuk.invalid", a.st.UIDValidity, uid)
		a.mu.Unlock()
	}
	conv := m.From + "/" + m.root()
	a.remember(conv, m)
	text := m.Text
	if m.root() == m.ID && m.Subject != "" {
		text = "Subject: " + m.Subject + "\n\n" + text
	}
	msg := channel.Message{
		Conversation: conv, ID: m.ID, Sender: m.From, SenderName: m.FromName,
		Text: text, Direct: true,
	}
	for _, f := range m.Files {
		data := f.Data
		msg.Attachments = append(msg.Attachments, channel.Attachment{
			Name: f.Name, MediaType: f.MediaType, Size: int64(len(data)),
			Fetch: func(context.Context) ([]byte, error) { return data, nil },
		})
	}
	in.Deliver(ctx, msg)
}

// remember records how to reply in a thread: its subject and references.
func (a *Adapter) remember(conv string, m *incoming) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.st.Threads[conv]
	if t == nil {
		t = &thread{}
		a.st.Threads[conv] = t
	}
	if t.Subject == "" || m.Subject != "" {
		t.Subject = m.Subject
	}
	refs := m.References
	if len(refs) == 0 {
		refs = m.InReplyTo
	}
	t.Refs = capRefs(append(slices.Clone(refs), m.ID))
	t.Seen = time.Now()
}

func capRefs(refs []string) []string {
	refs = slices.Compact(refs)
	if len(refs) > maxRefs {
		refs = append(refs[:1], refs[len(refs)-maxRefs+1:]...)
	}
	return refs
}

// split takes a conversation apart: the peer's address and the thread's
// root message id.
func split(conv string) (peer, root string, ok bool) {
	at := strings.Index(conv, "@")
	if at < 0 {
		return "", "", false
	}
	slash := strings.Index(conv[at:], "/")
	if slash < 0 {
		return "", "", false
	}
	return conv[:at+slash], conv[at+slash+1:], true
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, replyTo string) error {
	return a.send(ctx, to, text, replyTo, nil)
}

func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	if caption == "" {
		caption = name
	}
	return a.send(ctx, to, caption, "", &file{Name: name, MediaType: mediaType, Data: data})
}

// send writes one reply in the thread: In-Reply-To the message being
// answered (or the newest one seen), References the thread's chain.
func (a *Adapter) send(ctx context.Context, to channel.Address, text, replyTo string, f *file) error {
	peer, root, ok := split(to.Conversation)
	if !ok {
		return channel.Undeliverable{Err: fmt.Errorf("email: bad conversation %q", to.Conversation)}
	}
	if err := a.loadState(); err != nil {
		return err
	}
	a.mu.Lock()
	t := a.st.Threads[to.Conversation]
	subject, refs := "", []string{root}
	if t != nil {
		subject, refs = t.Subject, slices.Clone(t.Refs)
	}
	a.mu.Unlock()
	if replyTo != "" && !slices.Contains(refs, replyTo) {
		refs = append(refs, replyTo)
	}
	parent := refs[len(refs)-1]
	if replyTo != "" {
		parent = replyTo
	}
	raw, id, err := a.compose(peer, subject, parent, refs, text, f)
	if err != nil {
		return err
	}
	if err := a.submit(ctx, peer, raw); err != nil {
		return err
	}
	a.mu.Lock()
	t = a.st.Threads[to.Conversation]
	if t == nil {
		t = &thread{Subject: subject}
		a.st.Threads[to.Conversation] = t
	}
	t.Refs = capRefs(append(refs, id))
	t.Seen = time.Now()
	a.saveLocked()
	a.mu.Unlock()
	return nil
}

func replySubject(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Re: your message"
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "re:") {
		return s
	}
	return "Re: " + s
}

func (a *Adapter) compose(peer, subject, parent string, refs []string, text string, f *file) ([]byte, string, error) {
	var h mail.Header
	h.SetAddressList("From", []*mail.Address{{Name: a.cfg.Name, Address: a.cfg.Address}})
	h.SetAddressList("To", []*mail.Address{{Address: peer}})
	h.SetSubject(replySubject(subject))
	h.SetDate(time.Now())
	_, domain, _ := strings.Cut(a.cfg.Address, "@")
	if err := h.GenerateMessageIDWithHostname(domain); err != nil {
		return nil, "", err
	}
	id, _ := h.MessageID()
	h.SetMsgIDList("In-Reply-To", []string{parent})
	h.SetMsgIDList("References", refs)
	// Marks the reply as automatic, so vacation responders do not answer it
	// and two agents cannot mail each other forever.
	h.Set("Auto-Submitted", "auto-replied")
	h.Set("MIME-Version", "1.0")

	var buf bytes.Buffer
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if f == nil {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w, err := mail.CreateSingleInlineWriter(&buf, h)
		if err != nil {
			return nil, "", err
		}
		if _, err := w.Write([]byte(text)); err != nil {
			return nil, "", err
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), id, nil
	}
	w, err := mail.CreateWriter(&buf, h)
	if err != nil {
		return nil, "", err
	}
	var th mail.InlineHeader
	th.Set("Content-Type", "text/plain; charset=utf-8")
	tw, err := w.CreateSingleInline(th)
	if err != nil {
		return nil, "", err
	}
	if _, err := tw.Write([]byte(text)); err != nil {
		return nil, "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	var ah mail.AttachmentHeader
	ah.Set("Content-Type", f.MediaType)
	ah.SetFilename(f.Name)
	aw, err := w.CreateAttachment(ah)
	if err != nil {
		return nil, "", err
	}
	if _, err := aw.Write(f.Data); err != nil {
		return nil, "", err
	}
	if err := aw.Close(); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), id, nil
}

// submit hands one message to the SMTP server. A recipient the server
// refuses outright (5xx) is undeliverable.
func (a *Adapter) submit(ctx context.Context, rcpt string, raw []byte) error {
	host, _, _ := net.SplitHostPort(a.cfg.SMTP)
	d := &net.Dialer{Timeout: 30 * time.Second}
	m := mode(a.cfg.SMTPTLS, a.cfg.SMTP, "465")
	var conn net.Conn
	var err error
	if m == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: a.tlsConfig(a.cfg.SMTP)}).DialContext(ctx, "tcp", a.cfg.SMTP)
	} else {
		conn, err = d.DialContext(ctx, "tcp", a.cfg.SMTP)
	}
	if err != nil {
		return fmt.Errorf("smtp %s: %w", a.cfg.SMTP, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp %s: %w", a.cfg.SMTP, err)
	}
	defer func() { _ = c.Close() }()
	_, domain, _ := strings.Cut(a.cfg.Address, "@")
	if err := c.Hello(domain); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	if m == "starttls" {
		if err := c.StartTLS(a.tlsConfig(a.cfg.SMTP)); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if a.cfg.Password != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(smtp.PlainAuth("", a.cfg.Username, a.cfg.Password, host)); err != nil {
				return fmt.Errorf("smtp auth as %s: %w", a.cfg.Username, err)
			}
		}
	}
	if err := c.Mail(a.cfg.Address); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := c.Rcpt(rcpt); err != nil {
		var terr *textproto.Error
		if errors.As(err, &terr) && terr.Code >= 500 {
			return channel.Undeliverable{Err: fmt.Errorf("smtp: %s refused: %w", rcpt, err)}
		}
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	return c.Quit()
}
