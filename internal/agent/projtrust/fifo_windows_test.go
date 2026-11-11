//go:build windows

package projtrust

import "errors"

// mkfifo: Windows has no named pipes on disk; the tests that need one skip.
func mkfifo(string) error { return errors.New("no FIFOs on Windows") }
