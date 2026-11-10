// Package testutil holds small helpers shared by tests.
package testutil

import (
	"runtime"
	"testing"
)

// UnixOnly skips t on Windows. reason names the Unix behaviour the test
// exercises (a /bin/sh script, permission bits, symlinks, process groups),
// which has a Windows counterpart tested elsewhere or none at all.
//
// Keep these rare: `git grep -n testutil.UnixOnly` is the Windows skip
// list.
func UnixOnly(t testing.TB, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix: " + reason)
	}
}
