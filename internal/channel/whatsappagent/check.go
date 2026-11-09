package whatsappagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/channel/metagraph"
)

// ErrKeyInUse means the key works but another program is polling with it,
// most likely a running `habakkuk serve`.
var ErrKeyInUse = errors.New("whatsappagent: the API key works, but another program is polling with it (is habakkuk serve already running with this key?)")

// ErrBadKey means the platform refused the API key.
var ErrBadKey = errors.New("whatsappagent: WhatsApp refused the API key; copy it again from the agent's chat (Chat info > API key); it changes when it is regenerated or WhatsApp is reinstalled")

// Check proves the API key works with one poll that returns at once. It
// passes no offset and saves none, and polling does not consume updates, so
// nothing is skipped: the adapter's first real poll still sees every
// message. It returns the ids (without "user:") of the people whose
// messages are waiting, in the order they wrote, so setup can let the
// creator in.
func (a *Adapter) Check(ctx context.Context) ([]string, error) {
	if a.cfg.Token == "" {
		return nil, errors.New("whatsappagent: no API key")
	}
	q := url.Values{"timeout": {"0"}, "limit": {strconv.Itoa(pollLimit)}}
	var ups updates
	if err := a.api.JSON(ctx, http.MethodGet, "/updates?"+q.Encode(), nil, &ups); err != nil {
		var e *metagraph.Error
		if errors.As(err, &e) {
			switch {
			case e.Status == http.StatusConflict || e.Code == codeReplaced:
				return nil, ErrKeyInUse
			case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Code == 190 ||
				(e.Status == http.StatusBadRequest && e.Code == codeInvalid):
				return nil, fmt.Errorf("%w (HTTP %d)", ErrBadKey, e.Status)
			}
		}
		return nil, err
	}
	var senders []string
	seen := map[string]bool{}
	for _, e := range ups.Entry {
		for _, c := range e.Changes {
			for _, m := range c.Value.Messages {
				id, ok := strings.CutPrefix(m.From, "user:")
				if ok && id != "" && !strings.Contains(id, ":") && !seen[id] {
					seen[id] = true
					senders = append(senders, id)
				}
			}
		}
	}
	return senders, nil
}
