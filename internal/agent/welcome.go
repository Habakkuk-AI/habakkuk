package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// welcomePendingFile marks an agent that `habakkuk setup` named (and maybe
// told the user's name) before it met its user. An identity or a profile
// alone means the agent and its user have met; with this file the first
// one-to-one chat still runs the welcome, which introduces the agent by
// the name setup gave it.
const welcomePendingFile = ".welcome"

// KeepWelcome makes the agent's next first chat run the welcome even
// though it already has an identity or a user profile. Setup calls it after
// writing them for an agent that has not met its user yet.
func KeepWelcome(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := ResetOnboarding(dir); err != nil {
		return err
	}
	note := fmt.Sprintf("named by habakkuk setup %s; the first chat still runs the welcome\n", time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(filepath.Join(dir, welcomePendingFile), []byte(note), 0o600)
}

func welcomePending(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, welcomePendingFile))
	return err == nil
}
