package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/ops/secrets"
)

// SecretValues returns the current values of every variable the config
// names as a secret, for log redaction. Unset ones are skipped.
func (a *App) SecretValues() []string {
	var out []string
	for _, name := range a.Cfg.SecretEnvNames() {
		if v := a.Secret(name); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// SecretLookup reads credentials the way an open App does (environment,
// then secrets.refs, then $HABAKKUK_HOME/.env), for commands that need a few
// values without opening the databases.
func SecretLookup(cfg *config.Config, home string, log *slog.Logger) func(string) string {
	dot := loadDotEnv(filepath.Join(home, ".env"))
	refs := secrets.New(cfg, home, log)
	return func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		if v := refs.Get(context.Background(), name); v != "" {
			return v
		}
		return dot[name]
	}
}
