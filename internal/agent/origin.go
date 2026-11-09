package agent

import (
	"context"
	"encoding/json"
)

// OriginFrom is the reply address of the turn running in ctx (its payload's
// Origin), so a tool that reports later, such as a meeting attendance,
// answers in the chat the turn came from, scheduled turns included.
func OriginFrom(ctx context.Context) json.RawMessage { return originFrom(ctx) }
