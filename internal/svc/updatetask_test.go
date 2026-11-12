package svc

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestUpdateTaskXML(t *testing.T) {
	b := updateTaskXML(`C:\Program Files\SIERRA\meshbridge\meshbridge.exe`,
		`apply-update -config "C:\ProgramData\SIERRA & co\meshbridge.toml"`)
	if !bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || len(b)%2 != 0 {
		t.Fatalf("not UTF-16LE with a BOM: % x", b[:4])
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 2; i < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	doc := string(utf16.Decode(u))

	var task struct {
		Settings struct {
			DisallowStartIfOnBatteries bool
			StopIfGoingOnBatteries     bool
			StartWhenAvailable         bool
			ExecutionTimeLimit         string
		}
		Principals struct {
			Principal struct{ UserId string }
		}
		Triggers struct {
			TimeTrigger struct {
				Repetition struct{ Interval string }
			}
		}
		Actions struct {
			Exec struct{ Command, Arguments string }
		}
	}
	d := xml.NewDecoder(strings.NewReader(doc))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil } // already decoded
	if err := d.Decode(&task); err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	s := task.Settings
	if s.DisallowStartIfOnBatteries || s.StopIfGoingOnBatteries || !s.StartWhenAvailable || s.ExecutionTimeLimit != "PT10M" {
		t.Errorf("settings: %+v", s)
	}
	if task.Principals.Principal.UserId != "S-1-5-18" {
		t.Errorf("runs as %q, want SYSTEM", task.Principals.Principal.UserId)
	}
	if task.Triggers.TimeTrigger.Repetition.Interval != "PT5M" {
		t.Errorf("interval %q", task.Triggers.TimeTrigger.Repetition.Interval)
	}
	if task.Actions.Exec.Command != `C:\Program Files\SIERRA\meshbridge\meshbridge.exe` ||
		task.Actions.Exec.Arguments != `apply-update -config "C:\ProgramData\SIERRA & co\meshbridge.toml"` {
		t.Errorf("action: %+v", task.Actions.Exec)
	}
}
