//go:build windows

package projtrust

import "os"

// openFile opens path for reading; the type is checked after opening.
func openFile(path string) (*os.File, error) { return os.Open(path) }
