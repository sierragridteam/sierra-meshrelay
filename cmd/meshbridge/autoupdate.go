package main

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/bridge"
	"github.com/sierragridteam/sierra-meshrelay/internal/update"
	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// autoUpdater acts on the relay's update_available: it downloads and
// verifies the release into the config directory, waits until nobody is
// using the node, then has the update applied (see applyStaged, per
// platform). Only an installed service applies updates; a bridge run by
// hand just logs the offer.
type autoUpdater struct {
	dir     string
	log     *slog.Logger
	bridge  *bridge.Bridge
	managed bool
	// allowHTTP permits a plain-HTTP release URL (a local test relay).
	allowHTTP bool
	// restart is how a Linux service gets its update applied: exit, and
	// systemd's ExecStartPre installs it before starting again.
	restart func()

	mu   sync.Mutex
	busy bool
	done map[string]bool
}

func (u *autoUpdater) offer(m meshproto.UpdateAvailable) {
	if !update.Newer(m.Version, version) {
		return
	}
	if !u.managed {
		u.log.Info("an update is available; install it by running the installer again", "version", m.Version)
		return
	}
	pu, err := url.Parse(m.URL)
	if err != nil || (pu.Scheme != "https" && !(u.allowHTTP && pu.Scheme == "http")) {
		u.log.Warn("ignoring an update offer with a bad URL", "version", m.Version)
		return
	}
	u.mu.Lock()
	if u.busy || u.done[m.Version] {
		u.mu.Unlock()
		return
	}
	u.busy = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
	}()

	u.log.Info("downloading update", "version", m.Version, "from", m.URL)
	st := &update.Stager{Dir: u.dir}
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		err = st.Stage(ctx, m.URL, m.Version)
		cancel()
		if err == nil {
			break
		}
		if attempt == 3 || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "checksum") {
			u.log.Error("update failed; staying on this version", "version", m.Version, "error", err)
			u.markDone(m.Version)
			return
		}
		u.log.Warn("update download failed; retrying", "version", m.Version, "error", err)
		time.Sleep(time.Duration(attempt) * time.Minute)
	}
	u.markDone(m.Version)
	u.log.Info("update downloaded and verified", "version", m.Version)
	for !u.bridge.Idle() {
		time.Sleep(15 * time.Second)
	}
	u.log.Info("installing update now that the node is idle", "version", m.Version)
	applyStaged(u)
}

func (u *autoUpdater) markDone(v string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done == nil {
		u.done = map[string]bool{}
	}
	u.done[v] = true
}

// watchStalled warns, in the logs the relay keeps, when an update marked
// ready still hasn't been installed: after first, then every later. Whatever
// installs it restarts the bridge, which ends the watch; so does the staged
// update going away. notRun says why, when apply-update hasn't recorded an
// attempt at this version.
func (u *autoUpdater) watchStalled(first, later time.Duration, notRun string) {
	start := time.Now()
	for wait := first; ; wait = later {
		time.Sleep(wait)
		if !u.reportStalled(time.Since(start), notRun) {
			return
		}
	}
}

// reportStalled logs one warning if the staged update is still waiting,
// and reports whether it is.
func (u *autoUpdater) reportStalled(waited time.Duration, notRun string) bool {
	staged, ready := update.Staged(u.dir)
	if !ready {
		return false
	}
	why := notRun
	if r, ok := update.ReadResult(u.dir); ok && r.Version == staged && r.Error != "" {
		why = "the last attempt failed: " + r.Error
	}
	u.log.Warn("update downloaded but not installed yet", "version", staged, "waiting", waited.Round(time.Minute),
		"why", why, "fix", `on the bridge's computer, run "meshbridge apply-update" as administrator`)
	return true
}

// reportApplied logs, once, what apply-update did with the last update
// (see update.Result): it ran outside the bridge, so this is how the relay
// hears about it.
func reportApplied(dir string, log *slog.Logger) {
	r, ok := update.TakeResult(dir)
	if !ok {
		return
	}
	if r.Installed {
		log.Info("update installed", "version", r.Version, "was", r.Was, "at", r.Time.Format(time.RFC3339))
		return
	}
	log.Error("update not installed", "version", r.Version, "error", r.Error, "at", r.Time.Format(time.RFC3339))
}
