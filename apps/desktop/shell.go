package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/core"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/icon"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/notify"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/server"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/supervisor"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

const approvalCategory = "habakkuk-approval"

// shell is the native side: windows, tray, app menu, hotkey and
// notifications. It implements core.UI.
type shell struct {
	ctrl     *core.Controller
	srv      *server.Server
	conns    *connections.Manager
	notifier *notifications.NotificationService
	log      *slog.Logger

	app  *application.App
	main *application.WebviewWindow
	ask  *application.WebviewWindow
	tray *application.SystemTray

	mu       sync.Mutex
	hotkey   string
	trayDot  *bool
	dirty    chan struct{}
	nativeOK bool // native notifications work (a bundled app on macOS)
	notifyUp bool // the notification service was started
}

var _ core.UI = (*shell)(nil)

// build creates the windows, menus, tray and hotkey before the app runs.
func (s *shell) build() {
	s.dirty = make(chan struct{}, 1)
	start := "/desktop/"
	if _, ok := s.conns.Active(); ok {
		start = "/desktop/#wait" // shows the dashboard as soon as it answers
	}
	s.main = s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "main", Title: "Habakkuk",
		Width: 1200, Height: 820, MinWidth: 420, MinHeight: 360,
		URL:              s.srv.EntryURL(start),
		BackgroundColour: windowBackground(false),
	})
	// Closing the window keeps Habakkuk in the tray; Quit is in the menus.
	s.main.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		s.main.Hide()
	})

	s.ask = s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "ask", Title: "Ask Habakkuk",
		Width: 640, Height: 440, MinWidth: 360, MinHeight: 200,
		URL:              s.srv.EntryURL("/desktop/ask.html"),
		BackgroundColour: windowBackground(false),
		Hidden:           true,
		AlwaysOnTop:      true,
		HideOnEscape:     true,
		HideOnFocusLost:  true,
		InitialPosition:  application.WindowCentered,
	})
	s.ask.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		s.ask.Hide()
	})

	if runtime.GOOS == "darwin" {
		s.app.Menu.SetApplicationMenu(s.appMenu())
	}

	s.tray = s.app.SystemTray.New()
	s.tray.SetTooltip("Habakkuk")
	s.setTrayIcon(false)
	s.tray.SetMenu(s.trayMenu(s.ctrl.Snapshot()))

	if err := s.SetHotkey(s.conns.Settings().Hotkey); err != nil {
		s.log.Warn("hotkey", "accelerator", s.conns.Settings().Hotkey, "err", err)
	}
	go s.trayLoop()
}

// started runs once the app is up: notifications and start at login.
// Nothing here may block: asking for notification permission waits for
// the user to answer the system prompt.
func (s *shell) started() {
	// The system appearance is known once the app runs.
	s.setBackgrounds()
	s.app.Event.OnApplicationEvent(events.Common.ThemeChanged, func(*application.ApplicationEvent) { s.setBackgrounds() })
	s.notifier.OnNotificationResponse(s.onNotification)
	go s.startNotifications()
	go func() {
		want := s.conns.Settings().StartAtLogin
		if on, err := s.app.Autostart.IsEnabled(); err == nil && on != want {
			if err := s.SetStartAtLogin(want); err != nil {
				s.log.Warn("start at login", "err", err)
			}
		}
	}()
	s.Changed()
}

// startNotifications starts native notifications with the approve and
// deny actions; when the platform refuses (macOS without an app bundle, no
// notification daemon on Linux), notices go through fallbackNotify.
func (s *shell) startNotifications() {
	if err := s.notifier.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		s.log.Info("native notifications unavailable; using the fallback", "err", err)
		return
	}
	s.mu.Lock()
	s.notifyUp = true
	s.mu.Unlock()
	if err := s.notifier.RegisterNotificationCategory(notifications.NotificationCategory{
		ID: approvalCategory,
		Actions: []notifications.NotificationAction{
			{ID: "approve", Title: "Approve"},
			{ID: "deny", Title: "Deny", Destructive: true},
		},
	}); err != nil {
		s.log.Info("notification actions unavailable", "err", err)
	}
	ok, err := s.notifier.RequestNotificationAuthorization()
	if err != nil || !ok {
		s.log.Info("notifications not authorized; using the fallback", "err", err)
		return
	}
	s.mu.Lock()
	s.nativeOK = true
	s.mu.Unlock()
}

func (s *shell) stopNotifications() {
	s.mu.Lock()
	up := s.notifyUp
	s.mu.Unlock()
	if up {
		if err := s.notifier.ServiceShutdown(); err != nil {
			s.log.Warn("notifications", "err", err)
		}
	}
}

func (s *shell) appMenu() *application.Menu {
	m := application.NewMenu()
	m.AddRole(application.AppMenu)
	file := m.AddSubmenu("File")
	file.Add("New Chat").SetAccelerator("CmdOrCtrl+N").OnClick(func(*application.Context) { s.newChat() })
	file.Add("Quick Ask").OnClick(func(*application.Context) { s.toggleAsk() })
	file.Add("Connections…").SetAccelerator("CmdOrCtrl+,").OnClick(func(*application.Context) { s.ShowMain("/desktop/") })
	file.AddSeparator()
	file.Add("Close Window").SetAccelerator("CmdOrCtrl+W").OnClick(func(*application.Context) {
		if s.ask.IsFocused() {
			s.ask.Hide()
		} else {
			s.main.Hide()
		}
	})
	m.AddRole(application.EditMenu) // copy and paste in the page
	m.AddRole(application.ViewMenu)
	m.AddRole(application.WindowMenu)
	return m
}

// ShowMain shows the main window, at path when one is given.
func (s *shell) ShowMain(path string) {
	application.InvokeAsync(func() {
		if path != "" {
			s.main.SetURL(s.srv.EntryURL(path))
		}
		s.main.Show()
		s.main.Focus()
	})
}

func (s *shell) newChat() {
	s.ShowMain("/app/#/chat")
	// The dashboard has no address for a new chat; press its button once
	// the page is there.
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.main.ExecJS(`(function(){var n=0;var t=setInterval(function(){` +
			`var b=document.querySelector(".convs .top button.primary");` +
			`if(b||++n>50){clearInterval(t);if(b)b.click();}},100);})();`)
	}()
}

func (s *shell) toggleAsk() {
	application.InvokeAsync(func() {
		if s.ask.IsVisible() && s.ask.IsFocused() {
			s.ask.Hide()
			return
		}
		s.ask.Center()
		s.ask.Show()
		s.ask.Focus()
		s.ask.ExecJS(`document.getElementById("q")&&document.getElementById("q").focus()`)
	})
}

func (s *shell) MainFocused() bool { return s.main.IsVisible() && s.main.IsFocused() }
func (s *shell) AskVisible() bool  { return s.ask.IsVisible() }

// SetHotkey binds the quick ask window to accel, keeping the old binding
// when the new one is refused.
func (s *shell) SetHotkey(accel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if accel == s.hotkey {
		return nil
	}
	old := s.hotkey
	if old != "" {
		if err := s.app.GlobalShortcut.Unregister(old); err != nil {
			s.log.Warn("hotkey", "unregister", old, "err", err)
		}
	}
	if err := s.app.GlobalShortcut.Register(accel, s.toggleAsk); err != nil {
		if old != "" {
			if rerr := s.app.GlobalShortcut.Register(old, s.toggleAsk); rerr != nil {
				s.log.Warn("hotkey", "restore", old, "err", rerr)
			}
		}
		return err
	}
	s.hotkey = accel
	s.Changed()
	return nil
}

func (s *shell) SetStartAtLogin(on bool) error {
	if on {
		return s.app.Autostart.Enable()
	}
	return s.app.Autostart.Disable()
}

// Notify shows n natively, or through the fallback when the app is not
// bundled (macOS only shows notifications for a bundle with an identifier).
func (s *shell) Notify(n notify.Notice) {
	s.mu.Lock()
	native := s.nativeOK
	s.mu.Unlock()
	if native {
		opts := notifications.NotificationOptions{
			ID: n.Key, Title: n.Title, Body: n.Body, ThreadID: n.Conversation,
			Data: map[string]any{"kind": string(n.Kind), "approval": n.Approval, "conversation": n.Conversation},
		}
		var err error
		if n.Kind == notify.Approval {
			opts.CategoryID = approvalCategory
			err = s.notifier.SendNotificationWithActions(opts)
		} else {
			err = s.notifier.SendNotification(opts)
		}
		if err == nil {
			return
		}
		s.log.Warn("notification", "err", err)
	}
	fallbackNotify(n.Title, n.Body)
}

func (s *shell) onNotification(res notifications.NotificationResult) {
	if res.Error != nil {
		s.log.Warn("notification response", "err", res.Error)
		return
	}
	info := res.Response.UserInfo
	str := func(k string) string { v, _ := info[k].(string); return v }
	kind, approval, conv := str("kind"), str("approval"), str("conversation")
	switch res.Response.ActionIdentifier {
	case "approve", "deny":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.ctrl.Approve(ctx, approval, res.Response.ActionIdentifier == "approve"); err != nil {
			s.log.Warn("answer approval", "id", approval, "err", err)
			s.Notify(notify.Notice{Kind: notify.Reply, Key: "answer-failed:" + approval,
				Title: "Could not answer the approval", Body: err.Error()})
		}
	default: // the notification itself was clicked
		switch {
		case kind == string(notify.Approval):
			s.ShowMain("/app/#/approvals")
		case conv != "":
			s.ShowMain("/app/#/chat/" + conv)
		default:
			s.ShowMain("")
		}
	}
}

// fallbackNotify shows a notification without buttons: osascript on
// macOS, notify-send on Linux; nothing elsewhere.
func fallbackNotify(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		q := func(v string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"` }
		_ = exec.Command("/usr/bin/osascript", "-e", "display notification "+q(body)+" with title "+q(title)).Run()
	case "linux":
		if p, err := exec.LookPath("notify-send"); err == nil {
			_ = exec.Command(p, "--app-name=Habakkuk", title, body).Run()
		}
	}
}

// Changed asks for the tray to be redrawn; redraws are coalesced.
func (s *shell) Changed() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *shell) trayLoop() {
	for range s.dirty {
		time.Sleep(150 * time.Millisecond)
		st := s.ctrl.Snapshot()
		menu := s.trayMenu(st)
		application.InvokeAsync(func() {
			s.tray.SetMenu(menu)
			s.setTrayIcon(st.Status.Approvals > 0 || st.Problem != "" || (st.Active != "" && !st.Status.Connected))
			label := ""
			if st.Status.Approvals > 0 {
				label = fmt.Sprint(st.Status.Approvals)
			}
			s.tray.SetLabel(label)
			s.tray.SetTooltip("Habakkuk: " + statusLine(st))
		})
	}
}

// setBackgrounds matches what the windows show before their pages paint to
// the system's light or dark appearance.
func (s *shell) setBackgrounds() {
	bg := windowBackground(s.app.Env.IsDarkMode())
	s.main.SetBackgroundColour(bg)
	s.ask.SetBackgroundColour(bg)
}

func (s *shell) setTrayIcon(dot bool) {
	if s.trayDot != nil && *s.trayDot == dot {
		return
	}
	s.trayDot = &dot
	if runtime.GOOS == "darwin" {
		s.tray.SetTemplateIcon(icon.Tray(dot))
	} else {
		s.tray.SetIcon(icon.App(64))
	}
}

func statusLine(st core.State) string {
	switch {
	case st.Active == "":
		return "no connection"
	case st.Problem != "":
		return st.ActiveName + ": " + shorten(st.Problem, 60)
	case !st.Status.Connected && st.Status.Unauthorized:
		return st.ActiveName + ": wrong or missing token"
	case !st.Status.Connected:
		return "connecting to " + st.ActiveName + "…"
	default:
		return "connected to " + st.ActiveName
	}
}

func (s *shell) trayMenu(st core.State) *application.Menu {
	m := application.NewMenu()
	head := statusLine(st)
	m.Add(strings.ToUpper(head[:1]) + head[1:]).SetEnabled(false)
	if st.Status.Connected {
		ch := "No channels"
		if st.Status.Channels > 0 {
			ch = fmt.Sprintf("Channels: %d of %d connected", st.Status.ChannelsUp, st.Status.Channels)
		}
		m.Add(ch).SetEnabled(false)
		ap := m.Add(fmt.Sprintf("Approvals waiting: %d", st.Status.Approvals))
		ap.SetEnabled(st.Status.Approvals > 0)
		ap.OnClick(func(*application.Context) { s.ShowMain("/app/#/approvals") })
	}
	m.AddSeparator()
	m.Add("Open Habakkuk").OnClick(func(*application.Context) { s.ShowMain("") })
	m.Add("New Chat").OnClick(func(*application.Context) { s.newChat() })
	s.mu.Lock()
	hk := s.hotkey
	s.mu.Unlock()
	ask := "Quick Ask"
	if hk != "" {
		ask += "  (" + prettyAccel(hk) + ")"
	}
	m.Add(ask).OnClick(func(*application.Context) { s.toggleAsk() })
	m.AddSeparator()

	sub := m.AddSubmenu("Connections")
	for _, c := range st.Connections {
		id := c.ID
		sub.AddRadio(c.Name, c.ID == st.Active).OnClick(func(*application.Context) {
			if err := s.ctrl.Activate(id); err != nil {
				s.log.Warn("activate", "err", err)
			}
		})
	}
	sub.AddSeparator()
	sub.Add("Manage Connections…").OnClick(func(*application.Context) { s.ShowMain("/desktop/") })

	if st.Local != nil && !st.Local.External {
		if st.Local.State == supervisor.Stopped {
			m.Add("Resume hk serve").OnClick(func(*application.Context) { go s.localAction(s.ctrl.StartLocal) })
		} else {
			m.Add("Pause hk serve").OnClick(func(*application.Context) { go s.localAction(s.ctrl.StopLocal) })
		}
	}
	m.AddCheckbox("Start at Login", st.Settings.StartAtLogin).OnClick(func(ctx *application.Context) {
		on := !st.Settings.StartAtLogin
		if err := s.SetStartAtLogin(on); err != nil {
			s.log.Warn("start at login", "err", err)
		} else if _, err := s.conns.UpdateSettings(func(x *connections.Settings) { x.StartAtLogin = on }); err != nil {
			s.log.Warn("save settings", "err", err)
		}
		s.Changed()
	})
	m.AddSeparator()
	m.Add("Quit Habakkuk").OnClick(func(*application.Context) { s.app.Quit() })
	return m
}

func (s *shell) localAction(fn func() error) {
	if err := fn(); err != nil {
		s.log.Warn("hk serve", "err", err)
		fallbackNotify("Habakkuk", err.Error())
	}
	s.Changed()
}

// prettyAccel shows an accelerator the platform's way.
func prettyAccel(a string) string {
	cmd := "Ctrl"
	if runtime.GOOS == "darwin" {
		cmd = "Cmd"
	}
	return strings.NewReplacer("CmdOrCtrl", cmd, "CommandOrControl", cmd).Replace(a)
}

func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
