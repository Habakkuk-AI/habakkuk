package tools

import (
	"context"
	"sync"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// mediaKey carries the sink for media a running tool call attaches.
type mediaKey struct{}

type mediaSink struct {
	mu     sync.Mutex
	blocks []llm.Block
}

// AddMedia attaches image or document blocks (by blob reference) to the
// result of the tool call running in ctx, so the model sees them next to the
// result's text. It reports false when the caller does not collect media
// (the call was run with Execute rather than ExecuteMedia).
func AddMedia(ctx context.Context, blocks ...llm.Block) bool {
	sink, ok := ctx.Value(mediaKey{}).(*mediaSink)
	if !ok {
		return false
	}
	sink.mu.Lock()
	sink.blocks = append(sink.blocks, blocks...)
	sink.mu.Unlock()
	return true
}

// ExecuteMedia is Execute that also returns the media blocks the tool
// attached with AddMedia. An error result carries none.
func (r *Registry) ExecuteMedia(ctx context.Context, env *Env, call llm.Block) (llm.Block, []llm.Block) {
	sink := &mediaSink{}
	res := r.Execute(context.WithValue(ctx, mediaKey{}, sink), env, call)
	if res.IsError {
		return res, nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return res, sink.blocks
}
