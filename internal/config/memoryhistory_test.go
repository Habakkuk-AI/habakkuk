package config

import (
	"testing"
	"time"
)

func TestHistoryRetention(t *testing.T) {
	day := 24 * time.Hour
	var none *Memory
	for _, c := range []struct {
		m    *Memory
		want time.Duration
	}{
		{none, 90 * day},
		{&Memory{}, 90 * day},
		{&Memory{HistoryDays: 30}, 30 * day},
		{&Memory{HistoryDays: -1}, 0},
	} {
		if got := c.m.HistoryRetention(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.m, got, c.want)
		}
	}
	if errs := (&Memory{HistoryDays: -2}).validateHistory(); len(errs) != 1 {
		t.Errorf("-2 accepted: %v", errs)
	}
	if errs := (&Memory{HistoryDays: -1}).validateHistory(); len(errs) != 0 {
		t.Errorf("-1 refused: %v", errs)
	}
}
