package config

import (
	"errors"
	"time"
)

// DefaultHistoryDays is how long memory history is kept without
// memory.history_days.
const DefaultHistoryDays = 90

// HistoryRetention is how long changes to what agents remember are kept:
// memory.history_days, default 90 days. Zero keeps them all (-1 in the
// config). The newest version of every file that exists is always kept.
func (m *Memory) HistoryRetention() time.Duration {
	days := DefaultHistoryDays
	if m != nil && m.HistoryDays != 0 {
		days = m.HistoryDays
	}
	if days < 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

func (m *Memory) validateHistory() []error {
	if m.HistoryDays < -1 {
		return []error{errors.New("memory.history_days must be a number of days, or -1 to keep everything")}
	}
	return nil
}
