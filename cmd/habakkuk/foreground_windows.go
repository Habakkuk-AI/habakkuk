//go:build windows

package main

import "os"

// inForeground: Windows consoles have no foreground process group; the
// console check in openTTY is what there is.
func inForeground(*os.File) bool { return true }
