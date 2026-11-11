//go:build !linux && !darwin

package cutover

import "context"

// Inspect reads nothing on this system: ownership then rests on the
// command line, the pid files and the services.
func (Host) Inspect(context.Context, int) ProcInfo { return ProcInfo{} }
