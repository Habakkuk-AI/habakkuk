package app

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/consolidate"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/knowledge/skills"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
)

// openKnowledge opens the memory index, registers the memory and skill
// tools, and brings every agent's index up to date with its files, so notes
// edited while Habakkuk was down are searchable at once. With a memory
// section it also turns on semantic search and background review.
func (a *App) openKnowledge(ctx context.Context, home string) error {
	mem, err := memory.Open(ctx, home)
	if err != nil {
		return err
	}
	a.Memory = mem
	a.closers = append(a.closers, mem.Close)
	if mc := a.Cfg.Memory; mc != nil && mc.Embeddings != nil {
		mem.UseEmbedder(a.embedder(mc.Embeddings), a.Log)
	}
	a.Skills = &skills.Library{Home: home, Extra: a.Cfg.PluginSkillDirs()}
	if err := a.openMemoryHistory(ctx, home); err != nil {
		return err
	}
	memory.RegisterTools(a.Tools, mem)
	skills.RegisterTools(a.Tools, a.Skills)
	for _, name := range a.memoryAgents() {
		if err := mem.Reindex(ctx, name); err != nil {
			a.Log.Warn("memory reindex failed", "agent", name, "err", err)
		}
	}
	a.openConsolidation()
	return nil
}

// memoryAgents are the agents with the memory toolset.
func (a *App) memoryAgents() []string {
	var out []string
	for _, name := range a.Cfg.AgentNames() {
		if slices.Contains(a.Cfg.Agents[name].Toolsets, memory.Toolset) {
			out = append(out, name)
		}
	}
	return out
}

// embedder builds the embeddings client from the provider the config names.
func (a *App) embedder(e *config.Embeddings) memory.Embedder {
	p := a.Cfg.Providers[e.Provider] // validated at load
	key := ""
	for _, env := range p.KeysEnv {
		if key = a.Secret(env); key != "" {
			break
		}
	}
	kind := "openai"
	if p.Type == config.TypeGemini {
		kind = "gemini"
	}
	return &memory.HTTPEmbedder{Kind: kind, Provider: e.Provider, BaseURL: p.BaseURL, Key: key,
		Model: e.Model, Dimensions: e.Dimensions, Client: &http.Client{Timeout: time.Minute}}
}

// openConsolidation sets up background review for agents with the memory
// toolset when memory.review is on. Reviews use memory.review_model, or
// else the last model of the agent's chain (by convention its cheapest
// fallback), with the rest of the chain behind it.
func (a *App) openConsolidation() {
	mc := a.Cfg.Memory
	if mc == nil || !mc.Review {
		return
	}
	target := func(ref string) llm.Target {
		r, _ := config.ParseModelRef(ref) // validated at load
		return llm.Target{Endpoint: r.Provider, Model: r.Model}
	}
	chains := map[string][]llm.Target{}
	for _, name := range a.memoryAgents() {
		ac := a.Cfg.Agents[name]
		models := append([]string{ac.Model}, ac.Fallbacks...)
		first := models[len(models)-1]
		switch {
		case mc.ReviewModel != "":
			first = mc.ReviewModel
		case ac.FastModel != "":
			first = ac.FastModel // memory review is a chore
		}
		chain := []llm.Target{target(first)}
		for _, m := range models {
			if t := target(m); !slices.Contains(chain, t) {
				chain = append(chain, t)
			}
		}
		chains[name] = chain
	}
	loc := time.Local
	if a.Cfg.Timezone != "" {
		loc, _ = time.LoadLocation(a.Cfg.Timezone) // validated at load
	}
	at, nightly := mc.NightlyAt()
	a.Consolidate = &consolidate.Consolidator{
		Mem: a.Memory, Sessions: a.Sessions, Queue: a.Queue, Models: a.Router, Chains: chains,
		Idle: mc.ReviewIdle, Nightly: nightly, NightlyAt: at, StaleDays: max(mc.StaleDays, 0), Location: loc,
		Untrusted: a.untrustedOrProject, Log: a.Log,
	}
	if jc := a.Cfg.Judge; jc != nil {
		a.Consolidate.Judge, a.Consolidate.SkipBelow = a.judgeFor(jc.Memory), jc.SkipMemoryBelow
	}
}

// untrustedSession reports sessions whose messages come from a channel
// whose senders nobody vets: an email inbox.
func (a *App) untrustedSession(ctx context.Context, sessionID string) bool {
	if a.Gateway == nil {
		return false
	}
	conv, err := a.Gateway.Store.BySession(ctx, sessionID)
	if err != nil || conv == nil {
		return false
	}
	ch := a.Cfg.Channels[conv.Account]
	return ch != nil && ch.Type == "email"
}

// memoryPool runs review and nightly jobs, one at a time, in the serving
// process.
func (a *App) memoryPool() *jobs.Pool {
	return &jobs.Pool{
		Queue: a.Queue, Concurrency: 1, Lease: a.Cfg.Workers.Lease, Log: a.Log,
		Handlers: map[string]jobs.Handler{
			consolidate.KindReview:  a.Consolidate.HandleReview,
			consolidate.KindNightly: a.Consolidate.HandleNightly,
		},
	}
}

// MemoryLoop runs in `hk serve`: once a minute it queues retros that
// are due, records hand edits in the memory history, embeds notes that have
// no vector yet (new or hand-edited ones) and queues reviews of quiet
// sessions and the nightly pass.
func (a *App) MemoryLoop(ctx context.Context) {
	agents := a.memoryAgents()
	tick := func() {
		if err := a.Retro.Tick(ctx); err != nil && ctx.Err() == nil {
			a.Log.Warn("retro scheduling failed", "err", err)
		}
		a.scanMemoryHistory(ctx)
		if a.Memory.Embedder() == nil && a.Consolidate == nil {
			return
		}
		for _, name := range agents {
			if err := a.Memory.Reindex(ctx, name); err == nil {
				a.Memory.EmbedPending(ctx, name)
			}
		}
		if a.Consolidate != nil {
			if err := a.Consolidate.Tick(ctx); err != nil && ctx.Err() == nil {
				a.Log.Warn("memory review scheduling failed", "err", err)
			}
		}
	}
	tick()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// wireMemory gives chats /remember and /forget.
func (a *App) wireMemory() {
	if a.Gateway != nil {
		a.Gateway.Memory = memoryCommands{a.Memory}
	}
}

// memoryCommands implements channel.MemoryCommands.
type memoryCommands struct{ mem *memory.Store }

// Remember saves the user's words as a new note titled by its first words;
// it never replaces an existing note.
func (m memoryCommands) Remember(ctx context.Context, agentName, sessionID, text string) (string, error) {
	ctx = history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "/remember", Session: sessionID})
	note, _, err := m.mem.Put(ctx, agentName, memory.Note{
		Title: rememberTitle(text), Content: text, Tags: []string{"remembered"},
		Source: memory.SourceUser, Session: sessionID, Unique: true,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Saved to memory as %q (`%s`). /forget %s undoes it.", note.Title, note.Slug, note.Slug), nil
}

// Forget removes the note a slug or a query names. A query that matches
// several notes lists them rather than guessing.
func (m memoryCommands) Forget(ctx context.Context, agentName, query string) (string, error) {
	ctx = history.WithActor(ctx, history.Actor{By: history.ByUser, Tool: "/forget"})
	q := strings.TrimSpace(strings.Trim(query, "`"))
	if memory.ValidSlug(q) {
		if _, err := m.mem.Read(ctx, agentName, q); err == nil {
			if _, err := m.mem.Forget(ctx, agentName, q); err != nil {
				return "", err
			}
			return fmt.Sprintf("Forgot `%s` (it is in the memory trash if you change your mind).", q), nil
		}
	}
	hits, err := m.mem.Search(ctx, agentName, q, nil, 5)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No memory matches that.", nil
	}
	pick := -1
	if len(hits) == 1 {
		pick = 0
	}
	for i, h := range hits {
		if strings.EqualFold(h.Title, q) {
			pick = i
		}
	}
	if pick < 0 {
		var b strings.Builder
		b.WriteString("Several memories match; say which with /forget <slug>:")
		for _, h := range hits {
			fmt.Fprintf(&b, "\n- %s (`%s`)", h.Title, h.Slug)
		}
		return b.String(), nil
	}
	h := hits[pick]
	if _, err := m.mem.Forget(ctx, agentName, h.Slug); err != nil {
		return "", err
	}
	return fmt.Sprintf("Forgot %q (`%s`); it is in the memory trash if you change your mind.", h.Title, h.Slug), nil
}

// rememberTitle is a /remember note's title: its first line, cut to about
// eight words.
func rememberTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	words := strings.Fields(line)
	if len(words) > 8 {
		words = words[:8]
	}
	t := strings.TrimRight(strings.Join(words, " "), ".,;:!?")
	if r := []rune(t); len(r) > 60 {
		t = strings.TrimSpace(string(r[:60]))
	}
	if memory.Slugify(t) == "" {
		return "Remembered note"
	}
	return t
}

// snapshot lists an agent's memory and skills for a new session's prompt.
// Failures leave a listing empty rather than block the session.
func (a *App) snapshot(ctx context.Context, spec *agent.Spec) agent.Snapshot {
	var snap agent.Snapshot
	if lang := i18n.Pick(a.Cfg.Language); lang != i18n.Default {
		snap.Language = i18n.Name(lang)
	}
	if slices.Contains(spec.Toolsets, memory.Toolset) {
		snap.Memory = true
		recent, total, err := a.Memory.Recent(ctx, spec.Name, 50)
		if err != nil {
			a.Log.Warn("memory snapshot failed", "agent", spec.Name, "err", err)
		}
		for _, e := range recent {
			snap.Memories = append(snap.Memories, agent.MemoryRef{Title: e.Title, Slug: e.Slug, Untrusted: e.Untrusted})
		}
		snap.MemTotal = total
	}
	if slices.Contains(spec.Toolsets, skills.Toolset) {
		snap.Skills = true
		list, err := a.Skills.List(spec.Name)
		if err != nil {
			a.Log.Warn("skills snapshot failed", "agent", spec.Name, "err", err)
		}
		for _, s := range list {
			snap.SkillSet = append(snap.SkillSet, agent.SkillRef{Name: s.Name, Description: s.Description})
		}
	}
	return snap
}
