package app

import (
	"context"
	"path/filepath"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/channel/web"
)

// openWeb prepares the HTTP surface when the config has an http section. It
// opens web.db but does not listen; `hk serve` does that.
func (a *App) openWeb(ctx context.Context, home string) error {
	h := a.Cfg.HTTP
	st, err := web.OpenStore(ctx, filepath.Join(home, "web.db"))
	if err != nil {
		return err
	}
	a.closers = append(a.closers, func() { _ = st.Close() })
	s := &web.Server{
		Account: a.Cfg.WebAccountName(), Addr: h.Listen, TokenEnv: h.TokenEnv, Store: st, Log: a.Log,
		Hooks: map[string]*web.Hook{}, Retention: time.Duration(a.Cfg.Retention.Days) * 24 * time.Hour,
		PublicURL: h.PublicURL,
	}
	if h.TokenEnv != "" {
		s.Token = a.Secret(h.TokenEnv)
	}
	for name, wc := range h.Webhooks {
		hook, err := web.NewHook(name, wc, a.Secret(wc.SecretEnv))
		if err != nil {
			return err
		}
		s.Hooks[name] = hook
	}
	a.Web = s
	return nil
}
