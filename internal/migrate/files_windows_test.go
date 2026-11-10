package migrate

import "errors"

// mkfifo: Windows has no named pipes in the file system; the test that
// needs one is skipped there.
func mkfifo(string) error { return errors.New("no named pipes on Windows") }
