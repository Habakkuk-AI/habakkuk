package config

import "fmt"

// Judge configures a decision model: TypeSafe's Jev answers typed yes/no
// questions in well under a second for a fraction of a cent. Habakkuk uses
// it for its own small decisions; it never writes replies.
type Judge struct {
	Model     string `yaml:"model"`      // default jev-latest
	BaseURL   string `yaml:"base_url"`   // default https://api.typesafe.ai/v1
	KeyEnv    string `yaml:"key_env"`    // default TYPESAFE_API_KEY
	TimeoutMS int    `yaml:"timeout_ms"` // default 800
	// What it decides (each default true): which model a turn needs
	// (agents with a fast_model), whether a tool call is plainly routine
	// before the approval reviewer model is asked, and whether a chat is
	// worth a memory review.
	Routing   *bool `yaml:"routing"`
	Approvals *bool `yaml:"approvals"`
	Memory    *bool `yaml:"memory"`
	// ApproveAbove is how sure it must be that a call is routine to allow
	// it without the reviewer model (default 0.97). SkipMemoryBelow skips a
	// memory review when it is that unlikely the chat holds anything worth
	// keeping (default 0.15).
	ApproveAbove    float64 `yaml:"approve_above"`
	SkipMemoryBelow float64 `yaml:"skip_memory_below"`
}

func (j *Judge) applyDefaults() {
	t := true
	if j.Model == "" {
		j.Model = "jev-latest"
	}
	if j.BaseURL == "" {
		j.BaseURL = "https://api.typesafe.ai/v1"
	}
	if j.KeyEnv == "" {
		j.KeyEnv = "TYPESAFE_API_KEY"
	}
	if j.TimeoutMS == 0 {
		j.TimeoutMS = 800
	}
	for _, b := range []**bool{&j.Routing, &j.Approvals, &j.Memory} {
		if *b == nil {
			*b = &t
		}
	}
	if j.ApproveAbove == 0 {
		j.ApproveAbove = 0.97
	}
	if j.SkipMemoryBelow == 0 {
		j.SkipMemoryBelow = 0.15
	}
}

func (j *Judge) validate() []error {
	var errs []error
	if j.TimeoutMS < 100 || j.TimeoutMS > 10000 {
		errs = append(errs, fmt.Errorf("judge: timeout_ms must be between 100 and 10000"))
	}
	if j.ApproveAbove < 0.9 || j.ApproveAbove >= 1 {
		errs = append(errs, fmt.Errorf("judge: approve_above must be at least 0.9 and below 1"))
	}
	if j.SkipMemoryBelow < 0 || j.SkipMemoryBelow > 0.5 {
		errs = append(errs, fmt.Errorf("judge: skip_memory_below must be between 0 and 0.5"))
	}
	if !envName.MatchString(j.KeyEnv) {
		errs = append(errs, fmt.Errorf("judge: key_env %q is not a variable name", j.KeyEnv))
	}
	if !isHTTPURL(j.BaseURL) {
		errs = append(errs, fmt.Errorf("judge: base_url %q must be an http(s) URL", j.BaseURL))
	}
	return errs
}
