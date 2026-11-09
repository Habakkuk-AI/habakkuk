package config

import (
	"fmt"
	"strings"
)

// TTS turns text into speech for the speak tool and voice replies.
type TTS struct {
	Provider string `yaml:"provider"` // openai | elevenlabs | edge
	KeyEnv   string `yaml:"key_env"`  // variable holding the API key (edge needs none)
	BaseURL  string `yaml:"base_url"` // override the provider's endpoint
	Model    string `yaml:"model"`    // openai: gpt-4o-mini-tts; elevenlabs: eleven_multilingual_v2
	Voice    string `yaml:"voice"`    // openai: alloy; elevenlabs: a voice id; edge: en-US-AriaNeural
	Format   string `yaml:"format"`   // opus (default; voice notes) or mp3; edge always produces mp3
	// Instructions steer delivery (tone, pace) on models that take them.
	Instructions string `yaml:"instructions"`
	// MaxChars is the longest text spoken; longer replies stay text only.
	MaxChars int `yaml:"max_chars"`
}

// Images configures the image_generate tool.
type Images struct {
	Provider  string `yaml:"provider"`   // openai | gemini | fal
	KeyEnv    string `yaml:"key_env"`    // variable holding the API key
	BaseURL   string `yaml:"base_url"`   // override the provider's endpoint
	Model     string `yaml:"model"`      // openai: gpt-image-2; gemini: gemini-2.5-flash-image; fal: fal-ai/flux/dev
	EditModel string `yaml:"edit_model"` // fal only: the model used when edit_from is given
	Size      string `yaml:"size"`       // default size, e.g. 1024x1024
}

// Search configures the web_search tool, used by agents whose model has no
// hosted web search.
type Search struct {
	Provider string `yaml:"provider"` // brave | tavily | exa | searxng | duckduckgo
	KeyEnv   string `yaml:"key_env"`  // brave, tavily, exa
	URL      string `yaml:"url"`      // searxng: the instance; others: an endpoint override
}

var (
	defaultTTSKeys    = map[string]string{"openai": "OPENAI_API_KEY", "elevenlabs": "ELEVENLABS_API_KEY"}
	defaultImageKeys  = map[string]string{"openai": "OPENAI_API_KEY", "gemini": "GEMINI_API_KEY", "fal": "FAL_KEY"}
	defaultSearchKeys = map[string]string{"brave": "BRAVE_API_KEY", "tavily": "TAVILY_API_KEY", "exa": "EXA_API_KEY"}
)

func (c *Config) applyMediaDefaults() {
	if t := c.TTS; t != nil {
		if t.KeyEnv == "" {
			t.KeyEnv = defaultTTSKeys[t.Provider]
		}
		if t.Format == "" {
			t.Format = "opus"
		}
		if t.MaxChars == 0 {
			t.MaxChars = 2000
		}
	}
	if im := c.Images; im != nil && im.KeyEnv == "" {
		im.KeyEnv = defaultImageKeys[im.Provider]
	}
	if s := c.Search; s != nil && s.KeyEnv == "" {
		s.KeyEnv = defaultSearchKeys[s.Provider]
	}
	c.applyFeatureDefaults()
	c.applyVoiceDefaults()
	for _, ch := range c.Channels {
		if ch.VoiceReplies == "" {
			ch.VoiceReplies = "off"
		}
		if ch.VoiceText == nil {
			on := true
			ch.VoiceText = &on
		}
	}
}

func (c *Config) validateMedia() []error {
	errs := c.validateFeatures()
	errs = append(errs, c.validateVoice()...)
	if t := c.TTS; t != nil {
		switch t.Provider {
		case "openai", "elevenlabs", "edge":
		default:
			errs = append(errs, fmt.Errorf("tts: provider %q must be openai, elevenlabs or edge", t.Provider))
		}
		switch {
		case t.Format == "opus" || t.Format == "mp3":
		case t.Provider == "elevenlabs" && strings.Contains(t.Format, "_"): // a raw output_format such as mp3_44100_128
		default:
			errs = append(errs, fmt.Errorf("tts: format %q must be opus or mp3", t.Format))
		}
		if t.Provider == "elevenlabs" && t.Voice == "" {
			errs = append(errs, fmt.Errorf("tts: elevenlabs needs voice (a voice id)"))
		}
		if t.MaxChars < 0 {
			errs = append(errs, fmt.Errorf("tts: max_chars must not be negative"))
		}
	}
	if im := c.Images; im != nil {
		switch im.Provider {
		case "openai", "gemini", "fal":
		default:
			errs = append(errs, fmt.Errorf("images: provider %q must be openai, gemini or fal", im.Provider))
		}
	}
	if s := c.Search; s != nil {
		switch s.Provider {
		case "brave", "tavily", "exa", "duckduckgo":
		case "searxng":
			if s.URL == "" {
				errs = append(errs, fmt.Errorf("search: searxng needs url (your instance)"))
			}
		default:
			errs = append(errs, fmt.Errorf("search: provider %q must be brave, tavily, exa, searxng or duckduckgo", s.Provider))
		}
		if s.URL != "" && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			errs = append(errs, fmt.Errorf("search: url %q must start with http:// or https://", s.URL))
		}
	}
	for name, ch := range c.Channels {
		switch ch.VoiceReplies {
		case "off":
		case "mirror", "always":
			if c.TTS == nil {
				errs = append(errs, fmt.Errorf("channel %s: voice_replies %q needs a tts section", name, ch.VoiceReplies))
			}
		default:
			errs = append(errs, fmt.Errorf("channel %s: voice_replies %q must be off, mirror or always", name, ch.VoiceReplies))
		}
	}
	return errs
}
