package usage

import (
	"strings"

	"github.com/habakkuk-ai/habakkuk/internal/config"
)

// Price is a model's list price in US dollars per million tokens. Models
// that charge more for long prompts set Long, used when a call's prompt
// (input plus cached tokens) is over LongAbove.
type Price struct {
	Input, Output, CacheRead, CacheWrite float64
	LongAbove                            int64
	Long                                 *Price
}

func p(in, out, read, write float64) Price {
	return Price{Input: in, Output: out, CacheRead: read, CacheWrite: write}
}

func tiered(base Price, above int64, long Price) Price {
	base.LongAbove, base.Long = above, &long
	return base
}

// Prices is the built-in table of standard-tier list prices, keyed by model
// id; a key also matches longer ids that extend it ("claude-opus-5-5"
// matches "claude-opus-5-5-20261001"). Taken from the providers' pricing
// pages in November 2026; config `pricing:` overrides or adds entries.
var Prices = map[string]Price{
	// Anthropic (cache write is the 5-minute rate)
	"claude-fable-5-1":  p(10, 50, 0.25, 12.5),
	"claude-opus-5-5":   p(4, 20, 0.20, 5),
	"claude-sonnet-5-5": p(2, 10, 0.10, 2.5),
	"claude-haiku-5-5":  tiered(p(0.10, 0.50, 0.01, 0.125), 100_000, p(0.50, 2.50, 0.05, 0.625)),
	"claude-opus-5":     p(5, 25, 0.50, 6.25),
	"claude-opus-4-8":   p(5, 25, 0.50, 6.25),
	"claude-opus-4-7":   p(5, 25, 0.50, 6.25),
	"claude-opus-4-6":   p(5, 25, 0.50, 6.25),
	"claude-opus-4-5":   p(5, 25, 0.50, 6.25),
	"claude-sonnet-5":   p(2, 10, 0.20, 2.5),
	"claude-sonnet-4-6": p(3, 15, 0.30, 3.75),
	"claude-sonnet-4-5": p(3, 15, 0.30, 3.75),
	"claude-haiku-4-5":  p(1, 5, 0.10, 1.25),

	// OpenAI (long context is over 272k input tokens)
	"gpt-5.6-sol":   tiered(p(4, 20, 0.40, 5), 272_000, p(8, 30, 0.80, 10)),
	"gpt-5.6-terra": tiered(p(2, 12, 0.20, 2.5), 272_000, p(4, 18, 0.40, 5)),
	"gpt-5.6-luna":  tiered(p(0.20, 1.20, 0.02, 0.25), 272_000, p(0.40, 1.80, 0.04, 0.5)),
	"gpt-5.5":       tiered(p(5, 30, 0.50, 0), 272_000, p(10, 45, 1, 0)),
	"gpt-5.5-pro":   tiered(p(30, 180, 0, 0), 272_000, p(60, 270, 0, 0)),
	"gpt-5.4":       tiered(p(2.5, 15, 0.25, 0), 272_000, p(5, 22.5, 0.50, 0)),
	"gpt-5.4-pro":   tiered(p(30, 180, 0, 0), 272_000, p(60, 270, 0, 0)),
	"gpt-5.4-mini":  p(0.75, 4.5, 0.075, 0),
	"gpt-5.4-nano":  p(0.20, 1.25, 0.02, 0),
	"gpt-5.3-codex": p(1.75, 14, 0.175, 0),
	"gpt-5.2":       p(1.75, 14, 0.175, 0),
	"gpt-5.2-pro":   p(21, 168, 0, 0),
	"gpt-5.1":       p(1.25, 10, 0.125, 0),
	"gpt-5":         p(1.25, 10, 0.125, 0),
	"gpt-5-mini":    p(0.25, 2, 0.025, 0),
	"gpt-5-nano":    p(0.05, 0.40, 0.005, 0),
	"gpt-5-pro":     p(15, 120, 0, 0),

	// Google Gemini (output includes thinking)
	"gemini-4-argon":        p(2, 10, 0.10, 0),
	"gemini-3.8-flash":      p(0.75, 3.75, 0.075, 0),
	"gemini-3.6-flash":      p(0.75, 3.75, 0.075, 0),
	"gemini-3.5-flash-lite": p(0.30, 2.50, 0.03, 0),
	"gemini-3.1-pro":        tiered(p(2, 12, 0.20, 0), 200_000, p(4, 18, 0.40, 0)),
	"gemini-3.1-flash-lite": p(0.25, 1.50, 0.025, 0),
	"gemini-3-flash":        p(0.50, 3, 0.05, 0),
	"gemini-2.5-pro":        tiered(p(1.25, 10, 0.125, 0), 200_000, p(2.5, 15, 0.25, 0)),
	"gemini-2.5-flash":      p(0.30, 2.50, 0.03, 0),
	"gemini-2.5-flash-lite": p(0.10, 0.40, 0.01, 0),

	// TypeSafe decision model (input only; output is free)
	"jev": p(0.042, 0, 0, 0),
	// DeepSeek (peak-hour list price; off-peak is half)
	"deepseek-flash":    p(0.30, 1.20, 0.006, 0),
	"deepseek-v4-flash": p(0.30, 1.20, 0.006, 0), // the older name the API still accepts
	"deepseek-v4-pro":   p(1.32, 3.96, 0.044, 0),

	// xAI (prompts of 200k tokens or more bill at the higher rate)
	"grok-4.7":       tiered(p(2, 6, 0.50, 0), 199_999, p(4, 12, 1, 0)),
	"grok-4.6":       tiered(p(2, 6, 0.50, 0), 199_999, p(4, 12, 1, 0)),
	"grok-4.5":       tiered(p(2, 6, 0.30, 0), 199_999, p(4, 12, 0.60, 0)),
	"grok-4.3":       tiered(p(1.25, 2.5, 0.20, 0), 199_999, p(2.5, 5, 0.40, 0)),
	"grok-4.20":      tiered(p(1.25, 2.5, 0.20, 0), 199_999, p(2.5, 5, 0.40, 0)),
	"grok-build-0.1": tiered(p(1, 2, 0.20, 0), 199_999, p(2, 4, 0.40, 0)),

	// Mistral
	"mistral-large-4": p(0.70, 2.10, 0.07, 0),
	"mistral-small-4": p(0.15, 0.60, 0.015, 0),
}

// Pricer prices calls: config overrides first, then the built-in table.
// Calls through a subscription login cost nothing per token.
type Pricer struct {
	Overrides    map[string]*config.Price // "<provider>/<model>" or model id
	Subscription map[string]bool          // provider names that use a login
}

// NewPricer builds a pricer from the config.
func NewPricer(cfg *config.Config) *Pricer {
	pr := &Pricer{Overrides: cfg.Pricing, Subscription: map[string]bool{}}
	for name, p := range cfg.Providers {
		if p != nil && p.Auth != "" {
			pr.Subscription[name] = true
		}
	}
	return pr
}

// Lookup returns the price of a model on a provider.
func (pr *Pricer) Lookup(provider, model string) (Price, bool) {
	if pr != nil && len(pr.Overrides) > 0 {
		if o := pr.Overrides[provider+"/"+model]; o != nil {
			return fromConfig(o), true
		}
		over := map[string]Price{}
		for k, o := range pr.Overrides {
			if o != nil && !strings.Contains(k, "/") {
				over[strings.ToLower(k)] = fromConfig(o)
			}
		}
		if p, ok := match(over, model); ok {
			return p, true
		}
	}
	return match(Prices, model)
}

func fromConfig(o *config.Price) Price {
	return Price{Input: o.Input, Output: o.Output, CacheRead: o.CacheRead, CacheWrite: o.CacheWrite}
}

// Cost is a call's price in dollars. ok is false when the model has no
// known price; subscription calls cost 0.
func (pr *Pricer) Cost(c Call) (cost float64, ok bool) {
	if pr != nil && pr.Subscription[c.Provider] {
		return 0, true
	}
	price, ok := pr.Lookup(c.Provider, c.Model)
	if !ok {
		return 0, false
	}
	if price.Long != nil && c.Prompt() > price.LongAbove {
		price = *price.Long
	}
	return (float64(c.InputTokens)*price.Input + float64(c.OutputTokens)*price.Output +
		float64(c.CacheReadTokens)*price.CacheRead + float64(c.CacheWriteTokens)*price.CacheWrite) / 1e6, true
}

// regionPrefixes and vendorPrefixes are stripped from Bedrock-style ids
// ("us.anthropic.claude-opus-5-5-v1:0").
var (
	regionPrefixes = []string{"global.", "us.", "eu.", "apac.", "jp.", "au.", "ca."}
	vendorPrefixes = []string{"anthropic.", "amazon.", "meta.", "mistral.", "deepseek.", "openai.", "google.", "cohere.", "qwen.", "moonshot."}
)

// match finds the longest key that is the model id or a prefix of it ending
// at a separator. It also tries the id without a routing prefix
// ("anthropic/claude-..." on a gateway, "models/gemini-...") and with dots
// in version numbers as dashes ("claude-sonnet-4.5").
func match(table map[string]Price, model string) (Price, bool) {
	m := strings.ToLower(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	for _, pre := range regionPrefixes {
		m = strings.TrimPrefix(m, pre)
	}
	for _, pre := range vendorPrefixes {
		m = strings.TrimPrefix(m, pre)
	}
	for _, cand := range []string{m, strings.ReplaceAll(m, ".", "-")} {
		best, bestLen := Price{}, -1
		for k, p := range table {
			if len(k) > bestLen && (cand == k || strings.HasPrefix(cand, k) && strings.ContainsRune("-:@_", rune(cand[len(k)]))) {
				best, bestLen = p, len(k)
			}
		}
		if bestLen >= 0 {
			return best, true
		}
	}
	return Price{}, false
}
