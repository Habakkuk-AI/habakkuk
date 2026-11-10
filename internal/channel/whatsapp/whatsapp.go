// Package whatsapp is the WhatsApp adapter, a linked device on the user's
// number. Its credentials live in a SQLite database: every change is a
// transaction, so a full disk or a crash leaves the previous state intact
// instead of a truncated credentials file. Owner-only backups are taken on
// connect (at most hourly) and daily, keeping five recent copies and one a
// day for a week.
package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"

	"github.com/habakkuk-ai/habakkuk/internal/channel"
	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// MaxAge drops messages older than this when they arrive late (after an
// outage), rather than answering yesterday's questions out of the blue.
const MaxAge = 24 * time.Hour

type Adapter struct {
	DBPath    string
	BackupDir string
	Account   string
	Log       *slog.Logger

	mu        sync.Mutex
	db        *sql.DB
	container *sqlstore.Container
	client    *whatsmeow.Client
}

func New(account, dbPath, backupDir string, log *slog.Logger) *Adapter {
	return &Adapter{Account: account, DBPath: dbPath, BackupDir: backupDir, Log: log}
}

func (a *Adapter) MaxLen() int               { return 4000 }
func (a *Adapter) Format(md string) string   { return channel.FormatWhatsApp(md) }
func (a *Adapter) StaleAfter() time.Duration { return 3 * time.Minute }

func (a *Adapter) open(ctx context.Context) (*sqlstore.Container, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.container != nil {
		return a.container, nil
	}
	db, err := store.Open(ctx, a.DBPath, nil)
	if err != nil {
		return nil, err
	}
	c := sqlstore.NewWithDB(db, "sqlite", logger{a.Log.With("whatsapp", a.Account)})
	if err := c.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp store: %w", err)
	}
	a.db, a.container = db, c
	return c, nil
}

// Close releases the credentials database.
func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.db != nil {
		a.db.Close()
		a.db, a.container = nil, nil
	}
}

func (a *Adapter) setClient(c *whatsmeow.Client) {
	a.mu.Lock()
	a.client = c
	a.mu.Unlock()
}

func (a *Adapter) connected() (*whatsmeow.Client, error) {
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() {
		return nil, errors.New("whatsapp is not connected")
	}
	return c, nil
}

func (a *Adapter) Run(ctx context.Context, in channel.Inbox) error {
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	dev, err := c.GetFirstDevice(ctx)
	if err != nil {
		return err
	}
	if dev.ID == nil {
		return channel.Fatal{Err: fmt.Errorf("whatsapp account %s is not linked: run `habakkuk whatsapp link -account %s`", a.Account, a.Account)}
	}
	cli := whatsmeow.NewClient(dev, logger{a.Log.With("whatsapp", a.Account)})
	failed := make(chan error, 1)
	fail := func(err error) {
		select {
		case failed <- err:
		default:
		}
	}
	cli.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Message:
			if m, ok := convert(cli, v); ok {
				in.Deliver(ctx, m)
			}
		case *events.Connected:
			in.Connected("+" + dev.ID.User)
		case *events.LoggedOut:
			fail(channel.Fatal{Err: fmt.Errorf("whatsapp logged out (reason %v): link again", v.Reason)})
		case *events.StreamReplaced:
			fail(channel.Fatal{Err: errors.New("whatsapp session opened elsewhere with the same credentials")})
		case *events.TemporaryBan:
			fail(channel.Fatal{Err: fmt.Errorf("whatsapp temporary ban: %v", v)})
		case *events.ConnectFailure:
			fail(fmt.Errorf("whatsapp connect failure: %v %s", v.Reason, v.Message))
		}
	})
	if err := cli.Connect(); err != nil {
		return fmt.Errorf("whatsapp connect: %w", err)
	}
	a.setClient(cli)
	defer func() {
		a.setClient(nil)
		cli.Disconnect()
	}()

	a.backup(ctx, reconnectBackupGap)
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	daily := time.NewTicker(24 * time.Hour)
	defer daily.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return err
		case <-beat.C:
			// whatsmeow reconnects by itself; the supervisor steps in only
			// if it stays disconnected past StaleAfter.
			if cli.IsConnected() && cli.IsLoggedIn() {
				in.Beat()
			}
		case <-daily.C:
			a.backup(ctx, 0)
		}
	}
}

// reconnectBackupGap is the least time between two backups taken on
// (re)connect, so a flapping connection does not write a copy each time.
const reconnectBackupGap = time.Hour

// backup copies the credentials database, unless a copy is younger than
// minAge.
func (a *Adapter) backup(ctx context.Context, minAge time.Duration) {
	a.mu.Lock()
	db := a.db
	a.mu.Unlock()
	if a.BackupDir == "" || db == nil {
		return
	}
	prefix := "whatsapp-" + a.Account
	if last := store.LatestBackupTime(a.BackupDir, prefix); minAge > 0 && !last.IsZero() && time.Since(last) < minAge {
		return
	}
	path, err := store.Backup(ctx, db, a.BackupDir, prefix, store.DefaultRetention)
	if err != nil {
		a.Log.Warn("whatsapp backup failed", "account", a.Account, "err", err)
		return
	}
	a.Log.Info("whatsapp credentials backed up", "account", a.Account, "path", path)
}

func convert(cli *whatsmeow.Client, v *events.Message) (channel.Message, bool) {
	info := v.Info
	if info.IsFromMe || info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer {
		return channel.Message{}, false
	}
	if !info.Timestamp.IsZero() && time.Since(info.Timestamp) > MaxAge {
		return channel.Message{}, false
	}
	msg := v.Message
	text := msg.GetConversation()
	ext := msg.GetExtendedTextMessage()
	if text == "" {
		text = ext.GetText()
	}
	var atts []channel.Attachment
	add := func(dm whatsmeow.DownloadableMessage, name, mediaType string, size uint64) {
		atts = append(atts, channel.Attachment{Name: name, MediaType: mediaType, Size: int64(size),
			Fetch: func(ctx context.Context) ([]byte, error) { return cli.Download(ctx, dm) }})
	}
	switch {
	case msg.GetImageMessage() != nil:
		im := msg.GetImageMessage()
		text = firstNonEmpty(text, im.GetCaption())
		add(im, "photo.jpg", im.GetMimetype(), im.GetFileLength())
	case msg.GetVideoMessage() != nil:
		vm := msg.GetVideoMessage()
		text = firstNonEmpty(text, vm.GetCaption())
		add(vm, "video.mp4", vm.GetMimetype(), vm.GetFileLength())
	case msg.GetDocumentMessage() != nil:
		dm := msg.GetDocumentMessage()
		text = firstNonEmpty(text, dm.GetCaption())
		add(dm, firstNonEmpty(dm.GetFileName(), "file"), dm.GetMimetype(), dm.GetFileLength())
	case msg.GetAudioMessage() != nil:
		am := msg.GetAudioMessage()
		add(am, "voice.ogg", am.GetMimetype(), am.GetFileLength())
	case msg.GetStickerMessage() != nil:
		sm := msg.GetStickerMessage()
		add(sm, "sticker.webp", sm.GetMimetype(), sm.GetFileLength())
	}
	if text == "" && len(atts) == 0 {
		return channel.Message{}, false
	}

	m := channel.Message{
		Conversation: info.Chat.ToNonAD().String(),
		ID:           info.ID,
		SenderName:   info.PushName,
		Direct:       !info.IsGroup,
		Attachments:  atts,
	}
	m.Sender, m.SenderAlt = senderIDs(context.Background(), cli.Store, info.Sender, info.SenderAlt)

	var own []string
	if cli != nil && cli.Store != nil && cli.Store.ID != nil {
		own = []string{cli.Store.ID.User, cli.Store.LID.User}
	}
	ci := contextInfo(msg)
	m.Quote = quote(ci, own, []string{m.Sender, m.SenderAlt}, m.SenderName)
	if info.IsGroup && own != nil {
		for _, j := range ci.GetMentionedJID() {
			if u := userOf(j); u != "" && contains(own, u) {
				m.Mentioned = true
				text = strings.TrimSpace(strings.ReplaceAll(text, "@"+u, ""))
			}
		}
		if p := userOf(ci.GetParticipant()); p != "" && contains(own, p) {
			m.Mentioned = true // a reply to one of the bot's messages
		}
	}
	m.Text = text
	return m, true
}

// contextInfo is a message's context: what it quotes and mentions. Text
// replies carry it on the extended text; media replies on the media.
func contextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	for _, ci := range []*waE2E.ContextInfo{
		msg.GetExtendedTextMessage().GetContextInfo(),
		msg.GetImageMessage().GetContextInfo(),
		msg.GetVideoMessage().GetContextInfo(),
		msg.GetDocumentMessage().GetContextInfo(),
		msg.GetAudioMessage().GetContextInfo(),
		msg.GetStickerMessage().GetContextInfo(),
	} {
		if ci != nil {
			return ci
		}
	}
	return nil
}

// quote reads the message a reply quotes. own are the bot's user ids, and
// sender the ids of whoever wrote the reply.
func quote(ci *waE2E.ContextInfo, own, sender []string, senderName string) *channel.Quote {
	text := quotedText(ci.GetQuotedMessage())
	if text == "" {
		return nil
	}
	q := &channel.Quote{Text: text}
	switch p := userOf(ci.GetParticipant()); {
	case p != "" && contains(own, p):
		q.Self = true
	case p != "" && contains(sender, p):
		q.From = firstNonEmpty(senderName, "themselves")
	}
	return q
}

// quotedText is what a quoted message said, or what it was when it had no
// text.
func quotedText(qm *waE2E.Message) string {
	if qm == nil {
		return ""
	}
	if t := firstNonEmpty(qm.GetConversation(), qm.GetExtendedTextMessage().GetText()); t != "" {
		return t
	}
	switch {
	case qm.GetImageMessage() != nil:
		return firstNonEmpty(qm.GetImageMessage().GetCaption(), "(photo)")
	case qm.GetVideoMessage() != nil:
		return firstNonEmpty(qm.GetVideoMessage().GetCaption(), "(video)")
	case qm.GetDocumentMessage() != nil:
		dm := qm.GetDocumentMessage()
		return firstNonEmpty(dm.GetCaption(), "(file "+firstNonEmpty(dm.GetFileName(), "without a name")+")")
	case qm.GetAudioMessage() != nil:
		return "(voice note)"
	case qm.GetStickerMessage() != nil:
		return "(sticker)"
	}
	return ""
}

// senderIDs returns the ids allow_from and pairing match a sender by: the
// phone number first, as people write it, and the LID as the alternative.
// WhatsApp often names a sender by LID alone; the store's mapping between
// LIDs and phone numbers then fills in the other.
func senderIDs(ctx context.Context, dev *wastore.Device, sender, alt types.JID) (string, string) {
	sender, alt = sender.ToNonAD(), alt.ToNonAD()
	if alt.IsEmpty() && dev != nil && dev.LIDs != nil {
		var other types.JID
		var err error
		switch sender.Server {
		case types.HiddenUserServer:
			other, err = dev.LIDs.GetPNForLID(ctx, sender)
		case types.DefaultUserServer:
			other, err = dev.LIDs.GetLIDForPN(ctx, sender)
		}
		if err == nil && !other.IsEmpty() {
			alt = other.ToNonAD()
		}
	}
	if sender.Server == types.HiddenUserServer && !alt.IsEmpty() {
		sender, alt = alt, sender // the phone number is what people put in allow_from
	}
	return sender.User, alt.User
}

func userOf(jid string) string {
	j, err := types.ParseJID(jid)
	if err != nil {
		return ""
	}
	return j.User
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x != "" && x == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a *Adapter) Send(ctx context.Context, to channel.Address, text, _ string) error {
	jid, err := parseTarget(to.Conversation)
	if err != nil {
		return err
	}
	cli, err := a.connected()
	if err != nil {
		return err
	}
	_, err = cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	return sendErr(err)
}

// Habakkuk's caps on files it sends; images over maxImage go as documents.
const (
	maxFile  = 100 << 20
	maxImage = 16 << 20
)

func (a *Adapter) MaxFileSize() int64 { return maxFile }

func (a *Adapter) SendFile(ctx context.Context, to channel.Address, name, mediaType string, data []byte, caption string) error {
	jid, err := parseTarget(to.Conversation)
	if err != nil {
		return err
	}
	cli, err := a.connected()
	if err != nil {
		return err
	}
	msg, err := fileMessage(ctx, cli.Upload, name, mediaType, data, caption)
	if err != nil {
		return err
	}
	_, err = cli.SendMessage(ctx, jid, msg)
	return sendErr(err)
}

type uploader func(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error)

// fileMessage uploads the encrypted file and builds the message for it: an
// image shown inline for JPEG and PNG, a document for anything else.
func fileMessage(ctx context.Context, upload uploader, name, mediaType string, data []byte, caption string) (*waE2E.Message, error) {
	var capt *string
	if caption != "" {
		capt = proto.String(caption)
	}
	if (mediaType == "image/jpeg" || mediaType == "image/png") && len(data) <= maxImage {
		up, err := upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return nil, fmt.Errorf("whatsapp upload: %w", err)
		}
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: capt, Mimetype: proto.String(mediaType),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
		}}, nil
	}
	up, err := upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return nil, fmt.Errorf("whatsapp upload: %w", err)
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Caption: capt, Mimetype: proto.String(mediaType), FileName: proto.String(name), Title: proto.String(name),
		URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
	}}, nil
}

func (a *Adapter) Typing(ctx context.Context, to channel.Address) error {
	jid, err := parseTarget(to.Conversation)
	if err != nil {
		return err
	}
	cli, err := a.connected()
	if err != nil {
		return err
	}
	return cli.SendChatPresence(ctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText)
}

// Link pairs this account with a phone. With a phone number it prints an
// 8-character code to type into WhatsApp (Linked devices > Link with phone
// number), which works over SSH; otherwise it prints a QR code and also
// writes it as a PNG to qrPath.
func (a *Adapter) Link(ctx context.Context, phone, qrPath string, out io.Writer) error {
	c, err := a.open(ctx)
	if err != nil {
		return err
	}
	dev, err := c.GetFirstDevice(ctx)
	if err != nil {
		return err
	}
	if dev.ID != nil {
		return fmt.Errorf("already linked to +%s; to relink, unlink this device in WhatsApp first and delete %s", dev.ID.User, a.DBPath)
	}
	cli := whatsmeow.NewClient(dev, logger{a.Log.With("whatsapp", a.Account)})
	qrs, err := cli.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := cli.Connect(); err != nil {
		return err
	}
	defer cli.Disconnect()
	asked := false
	for item := range qrs {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if phone != "" {
				if asked {
					continue
				}
				asked = true
				code, err := cli.PairPhone(ctx, strings.TrimPrefix(phone, "+"), true, whatsmeow.PairClientChrome, "Chrome (Linux)")
				if err != nil {
					return fmt.Errorf("pair with phone: %w", err)
				}
				fmt.Fprintf(out, "On the phone: WhatsApp > Settings > Linked devices > Link a device > Link with phone number instead.\nEnter this code: %s\n", code)
				continue
			}
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, out)
			if qrPath != "" {
				if img, err := qr.Encode(item.Code, qr.L); err == nil {
					if err := store.WriteFileAtomic(qrPath, img.PNG(), 0o600); err == nil {
						fmt.Fprintf(out, "QR code also saved to %s (refreshes every %s)\n", qrPath, item.Timeout)
					}
				}
			}
			fmt.Fprintln(out, "Scan with WhatsApp > Settings > Linked devices > Link a device.")
		case whatsmeow.QRChannelSuccess.Event:
			if qrPath != "" {
				_ = os.Remove(qrPath)
			}
			// Give the phone a moment to hand over keys before disconnecting.
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			fmt.Fprintf(out, "Linked to +%s.\n", cli.Store.ID.User)
			return nil
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing failed: %w", item.Error)
		default:
			return fmt.Errorf("pairing ended: %s", item.Event)
		}
	}
	return errors.New("pairing ended without success")
}

// logger adapts slog to whatsmeow's logger, keeping its chatter at debug.
type logger struct{ l *slog.Logger }

func (l logger) Warnf(msg string, args ...any)  { l.l.Warn(fmt.Sprintf(msg, args...)) }
func (l logger) Errorf(msg string, args ...any) { l.l.Error(fmt.Sprintf(msg, args...)) }
func (l logger) Infof(msg string, args ...any)  { l.l.Debug(fmt.Sprintf(msg, args...)) }
func (l logger) Debugf(msg string, args ...any) {}
func (l logger) Sub(module string) waLog.Logger { return logger{l.l.With("module", module)} }
