package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/knowledge/memory/history"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// An agent's identity is the name, emoji and personality its user gave it,
// in agents/<agent>/IDENTITY.md. It sits on top of SOUL.md: SOUL.md is what
// the owner wrote by hand, IDENTITY.md is what the user chose in chat.
//
// A new agent meets its user with a welcome: the first one-to-one chat
// session of an agent that has neither an identity nor a user profile gets
// the onboarding instructions in its prompt. Starting that session claims
// the onboarding (agents/<agent>/.onboarding), so the welcome happens once
// even when the user ignores the questions.

const (
	IdentityFile     = "IDENTITY.md"
	onboardingMarker = ".onboarding"
	maxIdentityName  = 40
	maxIdentityEmoji = 8
	maxIdentityStyle = 300
)

// Identity is what IDENTITY.md holds.
type Identity struct {
	Name  string `json:"name"`
	Emoji string `json:"emoji,omitempty"`
	Style string `json:"style,omitempty"` // the personality the user asked for
}

// ReadIdentity reads dir's IDENTITY.md; ok is false when there is none or
// it names no one.
func ReadIdentity(dir string) (id Identity, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, IdentityFile))
	if err != nil {
		return Identity{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(strings.TrimLeft(k, "-* "))) {
		case "name":
			id.Name = clipRunes(v, maxIdentityName)
		case "emoji":
			id.Emoji = clipRunes(v, maxIdentityEmoji)
		case "style", "personality":
			id.Style = clipRunes(v, maxIdentityStyle)
		}
	}
	return id, id.Name != ""
}

// WriteIdentity checks id and writes IDENTITY.md, which also marks the
// onboarding done.
func WriteIdentity(dir string, id Identity) error {
	data, err := formatIdentity(id)
	if err != nil {
		return err
	}
	return writeIdentityFile(dir, data)
}

// formatIdentity checks id and renders IDENTITY.md.
func formatIdentity(id Identity) ([]byte, error) {
	id.Name = strings.TrimSpace(id.Name)
	id.Emoji = strings.TrimSpace(id.Emoji)
	id.Style = strings.Join(strings.Fields(id.Style), " ")
	switch {
	case id.Name == "":
		return nil, errors.New("name is required")
	case utf8.RuneCountInString(id.Name) > maxIdentityName:
		return nil, fmt.Errorf("name is longer than %d characters", maxIdentityName)
	case strings.ContainsAny(id.Name, "\n\r:#"):
		return nil, errors.New("name must be one line without ':' or '#'")
	case utf8.RuneCountInString(id.Emoji) > maxIdentityEmoji || strings.ContainsAny(id.Emoji, "\n\r"):
		return nil, errors.New("emoji must be one emoji")
	case utf8.RuneCountInString(id.Style) > maxIdentityStyle:
		return nil, fmt.Errorf("style is longer than %d characters", maxIdentityStyle)
	}
	for _, r := range id.Emoji {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return nil, errors.New("emoji must be an emoji, not text")
		}
	}
	var b strings.Builder
	b.WriteString("# Identity\n\nChosen by the user in chat. Habakkuk reads the lines below; change them by asking the agent, or edit them here.\n\n")
	fmt.Fprintf(&b, "name: %s\n", id.Name)
	if id.Emoji != "" {
		fmt.Fprintf(&b, "emoji: %s\n", id.Emoji)
	}
	if id.Style != "" {
		fmt.Fprintf(&b, "style: %s\n", id.Style)
	}
	return []byte(b.String()), nil
}

// writeIdentityFile writes IDENTITY.md and marks the onboarding done.
func writeIdentityFile(dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A fresh, exclusive temp name: a link waiting at a fixed one would be
	// written through.
	f, err := os.CreateTemp(dir, "."+IdentityFile+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr != nil {
		return werr
	}
	if err := os.Rename(tmp, filepath.Join(dir, IdentityFile)); err != nil {
		return err
	}
	_ = claimOnboarding(dir)
	return nil
}

// NeedsOnboarding reports whether dir's agent has not met its user yet: no
// identity, no user profile, and no welcome started.
func NeedsOnboarding(dir string) bool {
	if dir == "" {
		return false
	}
	if welcomePending(dir) { // named by setup: welcome once, under that name
		_, err := os.Stat(filepath.Join(dir, onboardingMarker))
		return errors.Is(err, fs.ErrNotExist)
	}
	if _, ok := ReadIdentity(dir); ok {
		return false
	}
	if data, err := os.ReadFile(filepath.Join(dir, "USER.md")); err == nil && strings.TrimSpace(string(data)) != "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, onboardingMarker))
	return errors.Is(err, fs.ErrNotExist)
}

// claimOnboarding records that the welcome started. It reports whether this
// call was the one that did, so two chats opening at once welcome once.
func claimOnboarding(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, onboardingMarker), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_, _ = fmt.Fprintf(f, "welcome started %s\n", time.Now().UTC().Format(time.RFC3339))
	return f.Close() == nil
}

// ResetOnboarding forgets that the welcome happened, so the next chat
// session runs it again (an identity or profile still skips it).
func ResetOnboarding(dir string) error {
	err := os.Remove(filepath.Join(dir, onboardingMarker))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// DisplayName is the name the agent goes by in what Habakkuk writes about
// it (a chat's "Handing this to ..." line): its identity's, else its
// configured name with a capital letter.
func DisplayName(agentName, dir string) string { return displayName(agentName, dir) }

// displayName is the name the agent goes by: its identity's, else its
// configured name with a capital letter.
func displayName(agentName, dir string) string {
	if id, ok := ReadIdentity(dir); ok {
		return id.Name
	}
	r, size := utf8.DecodeRuneInString(agentName)
	return string(unicode.ToUpper(r)) + agentName[size:]
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// SessionOptions changes how NewSessionWith starts a session.
type SessionOptions struct {
	// Chat marks a one-to-one conversation with the user (a direct
	// message, the CLI or TUI chat): the first one an agent has gets the
	// welcome.
	Chat bool
	// Onboard runs the welcome even when the agent already met its user
	// (the /onboard command).
	Onboard bool
}

const identityToolName = "set_identity"

// RegisterIdentity adds set_identity to the "core" toolset.
func (r *Runner) RegisterIdentity(reg *tools.Registry) {
	reg.Register("core", identityTool{r})
}

// RegisterIdentityWithHistory adds set_identity, recording every change it
// makes to IDENTITY.md in hist.
func (r *Runner) RegisterIdentityWithHistory(reg *tools.Registry, hist *history.Log) {
	reg.Register("core", recordedIdentityTool{identityTool{r}, hist})
}

type identityTool struct{ r *Runner }

// recordedIdentityTool is set_identity with a memory history.
type recordedIdentityTool struct {
	identityTool
	hist *history.Log
}

func (t recordedIdentityTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	return t.run(ctx, env, input, t.hist)
}

func (identityTool) Risk() tools.Risk { return tools.RiskWrite }
func (identityTool) OwnState()        {}
func (identityTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: identityToolName,
		Description: "Set the name you go by, an optional signature emoji and the personality the user wants from you. " +
			"Call it when the user tells you what to call yourself or how to behave, including when they answer the welcome questions (keep your current name if they did not pick one). " +
			"It replaces the whole identity, so pass the fields that should stay. New sessions start with it.",
		Schema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"what the user calls you, e.g. Habakkuk"},
			"emoji":{"type":"string","description":"one emoji you sign with, optional"},
			"style":{"type":"string","description":"the personality in a phrase, e.g. warm, brief and a little dry"}},
			"required":["name"]}`),
	}
}

func (t identityTool) Run(ctx context.Context, env *tools.Env, input json.RawMessage) (string, error) {
	return t.run(ctx, env, input, nil)
}

func (t identityTool) run(ctx context.Context, env *tools.Env, input json.RawMessage, hist *history.Log) (string, error) {
	var id Identity
	if err := json.Unmarshal(input, &id); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	// The identity goes into every prompt, so planted text must not set it.
	if env.Taint.Marked() {
		return "", errors.New("external content entered this turn, so the identity cannot change now; ask the user to confirm it in their next message")
	}
	spec, ok := t.r.Agents[env.Agent]
	if !ok || spec.Dir == "" {
		return "", fmt.Errorf("agent %q has no directory to keep an identity in", env.Agent)
	}
	data, err := formatIdentity(id)
	if err != nil {
		return "", err
	}
	write := func() error { return writeIdentityFile(spec.Dir, data) }
	if hist == nil {
		err = write()
	} else {
		ctx = history.WithActor(ctx, history.Actor{By: history.ByAgent, Tool: identityToolName, Session: env.Session, Tainted: env.Taint.Marked()})
		path, aerr := filepath.Abs(filepath.Join(spec.Dir, IdentityFile))
		if aerr != nil {
			return "", aerr
		}
		target := history.Target{Agent: env.Agent, Kind: history.KindIdentity, Name: IdentityFile, Path: path}
		_, err = hist.Write(ctx, target, data, write)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("identity saved: you are %s from now on, and new sessions start with it", strings.TrimSpace(id.Name)), nil
}
