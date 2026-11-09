package config

import (
	"fmt"
	"time"
)

// Video configures the video_generate tool.
type Video struct {
	Provider   string        `yaml:"provider"`    // openai | gemini | fal
	KeyEnv     string        `yaml:"key_env"`     // variable holding the API key
	BaseURL    string        `yaml:"base_url"`    // override the provider's endpoint
	Model      string        `yaml:"model"`       // openai: sora-2; gemini: veo-3.0-generate-001; fal: fal-ai/veo3/fast
	ImageModel string        `yaml:"image_model"` // fal only: the model used when an image is given
	Size       string        `yaml:"size"`        // openai: WIDTHxHEIGHT (1280x720); gemini and fal: an aspect ratio (16:9)
	Seconds    int           `yaml:"seconds"`     // default length (default 8)
	Timeout    time.Duration `yaml:"timeout"`     // how long to wait for a video (default 10m)
}

var defaultVideoKeys = map[string]string{"openai": "OPENAI_API_KEY", "gemini": "GEMINI_API_KEY", "fal": "FAL_KEY"}

// DefaultToolSearchOver is the tool count above which an agent's catalog is
// searched rather than sent whole.
const DefaultToolSearchOver = 60

// applyFeatureDefaults fills defaults for video, tool search and reactions.
func (c *Config) applyFeatureDefaults() {
	if v := c.Video; v != nil {
		if v.KeyEnv == "" {
			v.KeyEnv = defaultVideoKeys[v.Provider]
		}
		if v.Seconds == 0 {
			v.Seconds = 8
		}
		if v.Timeout == 0 {
			v.Timeout = 10 * time.Minute
		}
	}
	for _, a := range c.Agents {
		if a.ToolSearchOver == 0 {
			a.ToolSearchOver = DefaultToolSearchOver
		}
	}
}

func (c *Config) validateFeatures() []error {
	var errs []error
	if v := c.Video; v != nil {
		switch v.Provider {
		case "openai", "gemini", "fal":
		default:
			errs = append(errs, fmt.Errorf("video: provider %q must be openai, gemini or fal", v.Provider))
		}
		if v.Seconds < 1 || v.Seconds > 60 {
			errs = append(errs, fmt.Errorf("video: seconds must be between 1 and 60"))
		}
		if v.Timeout < 0 {
			errs = append(errs, fmt.Errorf("video: timeout must not be negative"))
		}
	}
	for name, a := range c.Agents {
		if a.ToolSearchOver < -1 {
			errs = append(errs, fmt.Errorf("agent %s: tool_search_over must be a number of tools, or -1 for off", name))
		}
		if a.TitleModel != "" && a.TitleModel != "off" {
			if _, err := c.ResolveModel(a.TitleModel); err != nil {
				errs = append(errs, fmt.Errorf("agent %s: title_model: %w", name, err))
			}
		}
	}
	return errs
}
