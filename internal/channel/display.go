package channel

import (
	"context"
	"errors"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Display is how a channel shows a turn at work and an approval request:
// the two settings a person can flip while Habakkuk runs, from the
// dashboard, from the chat itself (/progress, /approvals) or by editing
// config.yaml. They are read through the gateway at the start of each turn
// and each prompt, never straight from the config the process started
// with, so a change needs no restart.
type Display struct {
	Progress        string `json:"progress"`         // config.ProgressPlain, ProgressDetailed or ProgressOff
	ApprovalsDetail string `json:"approvals_detail"` // config.ApprovalsSummary or ApprovalsFull
}

// displayOf is a channel's display settings with their defaults. The local
// web chat is the owner's own screen, beside the dashboard, and keeps the
// exact call in its approval requests unless it is set otherwise.
func displayOf(ch *config.Channel) Display {
	d := Display{Progress: ch.ProgressMode(), ApprovalsDetail: ch.ApprovalsDetailMode()}
	if ch != nil && !ch.MessagingChannel() && ch.ApprovalsDetail == "" {
		d.ApprovalsDetail = config.ApprovalsFull
	}
	return d
}

// Display is the account's display settings as they are now.
func (g *Gateway) Display(account string) Display {
	if v, ok := g.display.Load(account); ok {
		return v.(Display)
	}
	return displayOf(g.Channels[account])
}

// SetDisplay makes the account use ch's progress and approvals_detail from
// the next turn on. The config watcher calls it when config.yaml changes,
// and ChangeDisplay after it saved a change.
func (g *Gateway) SetDisplay(account string, ch *config.Channel) {
	if _, ok := g.Channels[account]; !ok {
		return
	}
	g.display.Store(account, displayOf(ch))
}

// ErrDisplayReadOnly means this gateway cannot save display settings.
var ErrDisplayReadOnly = errors.New("settings cannot be changed here")

// ChangeDisplay saves new display settings for an account ("" leaves one as
// it is) and applies them at once. It returns the settings in effect.
func (g *Gateway) ChangeDisplay(account, progress, approvalsDetail string) (Display, error) {
	if _, ok := g.Channels[account]; !ok {
		return Display{}, errors.New("no such channel")
	}
	if g.SaveDisplay == nil {
		return g.Display(account), ErrDisplayReadOnly
	}
	ch, err := g.SaveDisplay(account, progress, approvalsDetail)
	if err != nil {
		return g.Display(account), err
	}
	g.SetDisplay(account, ch)
	return g.Display(account), nil
}

// cmdProgress is /progress: show or change what this chat shows while the
// agent works.
func (g *Gateway) cmdProgress(_ context.Context, c *Call) {
	arg := strings.ToLower(strings.TrimSpace(c.Args))
	if arg == "" {
		c.Reply(c.T("progress.current", c.T("progress.name."+g.Display(c.To.Account).Progress)))
		return
	}
	if _, ok := config.NormalizeProgress(arg); !ok {
		c.Reply(c.T("progress.pick"))
		return
	}
	d, err := g.ChangeDisplay(c.To.Account, arg, "")
	if err != nil {
		g.Log.Warn("save progress setting failed", "channel", c.To.Account, "err", err)
		c.Reply(c.T("settings.failed"))
		return
	}
	c.Reply(c.T("progress.set", c.T("progress.name."+d.Progress)))
}

// cmdApprovals is /approvals: show or change how much this chat's approval
// requests show.
func (g *Gateway) cmdApprovals(_ context.Context, c *Call) {
	arg := strings.ToLower(strings.TrimSpace(c.Args))
	if arg == "" {
		c.Reply(c.T("approvals.current", c.T("approvals.name."+g.Display(c.To.Account).ApprovalsDetail)))
		return
	}
	if _, ok := config.NormalizeApprovalsDetail(arg); !ok {
		c.Reply(c.T("approvals.pick"))
		return
	}
	d, err := g.ChangeDisplay(c.To.Account, "", arg)
	if err != nil {
		g.Log.Warn("save approvals setting failed", "channel", c.To.Account, "err", err)
		c.Reply(c.T("settings.failed"))
		return
	}
	c.Reply(c.T("approvals.set", c.T("approvals.name."+d.ApprovalsDetail)))
}
