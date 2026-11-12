package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Result is what apply-update did with a staged update. apply-update runs
// outside the bridge (systemd's ExecStartPre, or a Windows scheduled task),
// so its own output only reaches the host's journal or log file. It leaves
// a Result in the config directory instead, and the bridge reports it to
// the relay, where admins can see it.
type Result struct {
	Time      time.Time `json:"time"`
	Version   string    `json:"version"` // the staged version
	Was       string    `json:"was"`     // the version that tried to install it
	Installed bool      `json:"installed"`
	Error     string    `json:"error,omitempty"`
}

const resultFile = "update-result.json"

// WriteResult records what apply-update did.
func WriteResult(dir string, r Result) error {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, resultFile), b, 0o644)
}

// ReadResult returns the last recorded Result, if there is one.
func ReadResult(dir string) (Result, bool) {
	var r Result
	b, err := os.ReadFile(filepath.Join(dir, resultFile))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return Result{}, false
	}
	return r, true
}

// TakeResult returns the last recorded Result and removes it, so it is
// reported once.
func TakeResult(dir string) (Result, bool) {
	r, ok := ReadResult(dir)
	_ = os.Remove(filepath.Join(dir, resultFile))
	return r, ok
}
