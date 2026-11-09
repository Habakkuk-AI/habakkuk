// Command habakkuk-desktop is Habakkuk Desktop: a native window around the
// dashboard of a Habakkuk running on this computer or elsewhere, with a tray
// icon, notifications for approvals and replies, and a quick ask window on
// a global hotkey. See docs/interfaces/desktop-app.md.
//
// It is its own Go module, so the habakkuk binary keeps building without CGO
// and without any GUI library.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/connections"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/core"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/keychain"
	"github.com/habakkuk-ai/habakkuk/apps/desktop/internal/server"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

var version = "0.3.0-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("habakkuk-desktop", version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "habakkuk-desktop:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath, err := connections.DefaultPath()
	if err != nil {
		return err
	}
	log, closeLog := openLog(filepath.Dir(cfgPath))
	defer closeLog()
	conns, err := connections.Open(cfgPath, keychain.Store{})
	if err != nil {
		return err
	}

	ctrl := &core.Controller{Conns: conns, Log: log, Version: version}
	srv := server.New(ctrl.API(), log)
	ctrl.Server = srv
	ln, err := srv.Listen(conns.Settings().Port)
	if err != nil {
		return fmt.Errorf("loopback server: %w", err)
	}
	if conns.Settings().Port != srv.Port() {
		// Keep the port, so the dashboard's settings (kept per origin) stay.
		if _, err := conns.UpdateSettings(func(s *connections.Settings) { s.Port = srv.Port() }); err != nil {
			log.Warn("save port", "err", err)
		}
	}
	handler := srv.Handler()
	if os.Getenv("HABAKKUK_DESKTOP_DEBUG") != "" {
		handler = logRequests(log, handler)
	}
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("loopback server stopped", "err", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notifier := notifications.New()
	sh := &shell{ctrl: ctrl, srv: srv, conns: conns, notifier: notifier, log: log}
	ctrl.UI = sh

	app := application.New(application.Options{
		Name:        "Habakkuk",
		Description: "Habakkuk Desktop",
		Icon:        appIcon(),
		Logger:      log,
		// The notification service is started by hand (shell.started): as
		// a registered service its failure, e.g. macOS refusing an app that
		// is not bundled, would stop the whole app.
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyRegular,
			// Closing the window keeps the app in the tray.
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               bundleID,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { sh.ShowMain("") },
		},
		OnShutdown: func() {
			sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer scancel()
			ctrl.Shutdown(sctx)
			sh.stopNotifications()
			_ = httpSrv.Close()
		},
	})
	sh.app = app
	sh.build()
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go ctrl.Start(ctx)
		sh.started()
	})
	return app.Run()
}

// openLog writes the app's log to desktop.log next to its config (and to
// stderr), starting the file afresh past 5 MB.
func openLog(dir string) (*slog.Logger, func()) {
	var w io.Writer = os.Stderr
	closeFn := func() {}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		path := filepath.Join(dir, "desktop.log")
		if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
			_ = os.Rename(path, path+".1")
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			w = io.MultiWriter(os.Stderr, f)
			closeFn = func() { f.Close() }
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})), closeFn
}

// logRequests logs every request to the loopback server (method, path,
// status), for HABAKKUK_DESKTOP_DEBUG=1. Query strings are left out: the
// entry URL carries the session key.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("loopback", "method", r.Method, "path", r.URL.Path, "status", rec.code)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) { s.code = code; s.ResponseWriter.WriteHeader(code) }

// Unwrap lets http.ResponseController reach Flush for event streams.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
