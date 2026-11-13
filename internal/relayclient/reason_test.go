package relayclient

import (
	"strings"
	"testing"
)

func TestReason(t *testing.T) {
	for in, want := range map[string]string{
		"this token is for gateway cabin, not ridge\n": "this token is for gateway cabin, not ridge",
		"first line\nsecond line":                      "first line",
		"<html><body>403 Forbidden</body></html>":      "",
		"evil\x1b[2Jclear\x07":                         "evil[2Jclear",
		"":                                             "",
		strings.Repeat("a", 300):                       strings.Repeat("a", 200) + "…",
	} {
		if got := reason(in); got != want {
			t.Errorf("reason(%q) = %q, want %q", in, got, want)
		}
	}
}
