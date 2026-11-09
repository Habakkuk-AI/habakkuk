package config

import (
	"fmt"
	"strings"
)

// Report modes for an agent's home chat (Agent.ReportHome).
const (
	ReportHomeOff     = "off"
	ReportHomeResults = "results"
	ReportHomeAll     = "all"
)

// applyHomeDefaults reports results to a home chat unless told otherwise.
func (c *Config) applyHomeDefaults() {
	for _, a := range c.Agents {
		if a != nil && a.Home != "" && a.ReportHome == "" {
			a.ReportHome = ReportHomeResults
		}
	}
}

// validateHomes checks that each agent's home chat names a configured
// channel and that its report mode is known.
func (c *Config) validateHomes() []error {
	var errs []error
	for _, name := range c.AgentNames() {
		a := c.Agents[name]
		if a == nil {
			continue
		}
		switch a.ReportHome {
		case "", ReportHomeOff:
		case ReportHomeResults, ReportHomeAll:
			if a.Home == "" {
				errs = append(errs, fmt.Errorf("agent %s: report_home %s needs home: CHANNEL:CONVERSATION, the chat to report to", name, a.ReportHome))
			}
		default:
			errs = append(errs, fmt.Errorf("agent %s: report_home %q must be off, results or all", name, a.ReportHome))
		}
		if a.Home == "" {
			continue
		}
		account, ok := HomeChannel(a.Home)
		if !ok {
			errs = append(errs, fmt.Errorf("agent %s: home %q: want CHANNEL:CONVERSATION or CHANNEL:CONVERSATION:THREAD (e.g. wagent:creator)", name, a.Home))
			continue
		}
		if c.Channels[account] == nil {
			errs = append(errs, fmt.Errorf("agent %s: home %q names channel %q, which is not configured (add it under channels)", name, a.Home, account))
		}
	}
	return errs
}

// HomeChannel returns the channel account of a home target, read the way
// `habakkuk send` reads CHANNEL:CONVERSATION[:THREAD].
func HomeChannel(home string) (string, bool) {
	account, rest, ok := strings.Cut(home, ":")
	conv, _, _ := strings.Cut(rest, ":")
	if !ok || account == "" || conv == "" {
		return "", false
	}
	return account, true
}
