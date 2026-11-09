package apptoken

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCacheRefreshesBeforeExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	n := 0
	c := &Cache{
		Now: func() time.Time { return now },
		Fetch: func(context.Context) (string, time.Duration, error) {
			n++
			return fmt.Sprint("t", n), 2 * time.Hour, nil
		},
	}
	ctx := context.Background()
	if tok, _ := c.Get(ctx); tok != "t1" {
		t.Fatalf("first = %s", tok)
	}
	now = now.Add(time.Hour)
	if tok, _ := c.Get(ctx); tok != "t1" {
		t.Fatalf("cached = %s", tok)
	}
	now = now.Add(56 * time.Minute) // inside the five-minute margin
	if tok, _ := c.Get(ctx); tok != "t2" {
		t.Fatalf("refreshed = %s", tok)
	}
	c.Reset("t1") // stale reset: keeps t2
	if tok, _ := c.Get(ctx); tok != "t2" {
		t.Fatalf("after stale reset = %s", tok)
	}
	c.Reset("t2")
	if tok, _ := c.Get(ctx); tok != "t3" {
		t.Fatalf("after reset = %s", tok)
	}
}
