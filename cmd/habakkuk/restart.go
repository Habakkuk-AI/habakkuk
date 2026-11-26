package main

import "errors"

// errRestart is what serve returns when the dashboard asked for a restart:
// it has stopped cleanly, and main starts the binary again (restartSelf).
var errRestart = errors.New("restart requested")
