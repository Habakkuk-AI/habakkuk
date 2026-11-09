package app

import (
	"context"
	"path/filepath"

	"github.com/habakkuk-ai/habakkuk/internal/channel/web"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/nodes"
)

// openNodes opens nodes.db and the node hub when the config has a nodes
// section: the node tools go into the core toolset, and the HTTP surface
// serves /nodes (devices) and /api/nodes (the dashboard). Only the process
// that serves HTTP has live nodes; others see them as offline.
func (a *App) openNodes(ctx context.Context, home string) error {
	cfg := a.Cfg.Nodes
	if cfg == nil {
		return nil
	}
	st, err := nodes.OpenStore(ctx, filepath.Join(home, "nodes.db"))
	if err != nil {
		return err
	}
	a.closers = append(a.closers, func() { _ = st.Close() })
	hub := &nodes.Hub{Store: st, Log: a.Log, PairTTL: cfg.PairCodeTTL()}
	nodes.RegisterTools(a.Tools, hub, cfg)
	if a.Web != nil {
		a.Web.Nodes = &web.NodeEndpoints{Public: hub.Public(), Admin: hub.Admin()}
	}
	return nil
}
