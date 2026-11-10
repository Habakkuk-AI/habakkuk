package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

// Approval is a tool call waiting for a yes or no in a chat. The web
// dashboard lists them and can answer them in place of the chat.
type Approval struct {
	ID      string    `json:"id"`
	To      Address   `json:"to"`
	Session string    `json:"session"`
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"`
	Review  string    `json:"review,omitempty"` // why the reviewer handed it to a person
	Since   time.Time `json:"since"`
}

func approvalID(to Address) string {
	sum := sha256.Sum256([]byte(to.key()))
	return "apr_" + hex.EncodeToString(sum[:12])
}

// Approvals lists the approvals waiting now, oldest first.
func (g *Gateway) Approvals() []Approval {
	var out []Approval
	g.asked.Range(func(_, v any) bool {
		out = append(out, v.(Approval))
		return true
	})
	slices.SortFunc(out, func(a, b Approval) int { return a.Since.Compare(b.Since) })
	return out
}

// Decide answers a waiting approval as if the chat had said yes or no, and
// tells the chat. It reports false when no such approval is waiting.
func (g *Gateway) Decide(id string, ok bool) bool {
	for _, a := range g.Approvals() {
		if a.ID != id {
			continue
		}
		if !g.resolveApproval(a.To, ok) {
			return false
		}
		g.forgetAsked(a.To)
		to := a.To
		to.ReplyTo = ""
		if ok {
			g.reply(to, "Approved from the dashboard.")
		} else {
			g.reply(to, "Denied from the dashboard.")
		}
		return true
	}
	return false
}

// Conversations lists an account's conversations, most recent first.
func (s *Store) Conversations(ctx context.Context, account string, limit int) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT conversation, thread, agent, session_id, last_at FROM conversations
		WHERE account = ? ORDER BY last_at DESC LIMIT ?`, account, max(limit, 1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		c := Conversation{Address: Address{Account: account}}
		var last int64
		if err := rows.Scan(&c.Conversation, &c.Thread, &c.Agent, &c.SessionID, &last); err != nil {
			return nil, err
		}
		c.LastAt = time.UnixMilli(last)
		out = append(out, c)
	}
	return out, rows.Err()
}

// recordApproval notes a waiting approval for Approvals until forget runs.
func (g *Gateway) recordApproval(to Address, session string, tool, summary, review string) (forget func()) {
	key := to.key()
	g.asked.Store(key, Approval{
		ID: approvalID(to), To: to, Session: session, Tool: tool,
		Summary: strings.TrimSpace(summary), Review: strings.TrimSpace(review), Since: g.now(),
	})
	return func() { g.asked.Delete(key) }
}
