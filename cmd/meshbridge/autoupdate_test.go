package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sierragridteam/sierra-meshrelay/internal/update"
)

func stage(t *testing.T, dir, version string, ready bool) {
	t.Helper()
	d := filepath.Join(dir, update.StageDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "version"), []byte(version), 0o600); err != nil {
		t.Fatal(err)
	}
	if ready {
		if err := update.MarkReady(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func testLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestReportStalled(t *testing.T) {
	dir := t.TempDir()
	log, out := testLog()
	u := &autoUpdater{dir: dir, log: log}

	if u.reportStalled(time.Minute, "the task hasn't run it") {
		t.Fatal("stalled with nothing staged")
	}
	stage(t, dir, "v1.2.0", true)
	if !u.reportStalled(15*time.Minute, "the task hasn't run it") {
		t.Fatal("not stalled with an update ready")
	}
	if s := out.String(); !strings.Contains(s, "level=WARN") || !strings.Contains(s, "version=v1.2.0") ||
		!strings.Contains(s, "the task hasn't run it") || !strings.Contains(s, "apply-update") {
		t.Fatalf("warning:\n%s", s)
	}

	// A failed attempt at an older version doesn't explain this one.
	out.Reset()
	_ = update.WriteResult(dir, update.Result{Version: "v1.1.0", Error: "old trouble"})
	u.reportStalled(time.Hour, "the task hasn't run it")
	if strings.Contains(out.String(), "old trouble") {
		t.Fatalf("reported an older attempt:\n%s", out)
	}
	// A failed attempt at this one does.
	out.Reset()
	_ = update.WriteResult(dir, update.Result{Version: "v1.2.0", Error: "can't stop the service"})
	u.reportStalled(time.Hour, "the task hasn't run it")
	if s := out.String(); !strings.Contains(s, "the last attempt failed: can't stop the service") || strings.Contains(s, "hasn't run it") {
		t.Fatalf("warning:\n%s", s)
	}

	// Once installed (or discarded), the update isn't waiting any more.
	_ = update.Discard(dir)
	if u.reportStalled(time.Hour, "") {
		t.Fatal("stalled after the stage went away")
	}
}

func TestReportApplied(t *testing.T) {
	dir := t.TempDir()
	log, out := testLog()
	reportApplied(dir, log)
	if out.Len() != 0 {
		t.Fatalf("reported with no result:\n%s", out)
	}

	_ = update.WriteResult(dir, update.Result{Version: "v1.2.0", Was: "v1.1.0", Installed: true})
	reportApplied(dir, log)
	if s := out.String(); !strings.Contains(s, "level=INFO") || !strings.Contains(s, `msg="update installed"`) || !strings.Contains(s, "was=v1.1.0") {
		t.Fatalf("installed:\n%s", s)
	}
	out.Reset()
	reportApplied(dir, log)
	if out.Len() != 0 {
		t.Fatalf("reported twice:\n%s", out)
	}

	_ = update.WriteResult(dir, update.Result{Version: "v1.2.0", Was: "v1.1.0", Error: "the new binary didn't run"})
	reportApplied(dir, log)
	if s := out.String(); !strings.Contains(s, "level=ERROR") || !strings.Contains(s, "the new binary didn't run") {
		t.Fatalf("failed:\n%s", s)
	}
}
