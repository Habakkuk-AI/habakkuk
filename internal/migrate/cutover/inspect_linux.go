//go:build linux

package cutover

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// Inspect reads /proc: the environment and working folder of the user's
// own processes, and every process's control group.
func (Host) Inspect(_ context.Context, pid int) ProcInfo {
	dir := "/proc/" + strconv.Itoa(pid)
	var info ProcInfo
	if data, err := os.ReadFile(dir + "/environ"); err == nil {
		info.Env = map[string]string{}
		for _, kv := range strings.Split(string(data), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				info.Env[k] = v
			}
		}
	}
	info.Cwd, _ = os.Readlink(dir + "/cwd")
	if data, err := os.ReadFile(dir + "/cgroup"); err == nil {
		info.Cgroup = strings.TrimSpace(string(data))
	}
	return info
}
