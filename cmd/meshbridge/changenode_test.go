package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/sierragridteam/sierra-meshrelay/internal/companion"
	"github.com/sierragridteam/sierra-meshrelay/internal/control"
	"github.com/sierragridteam/sierra-meshrelay/internal/nodelink"
)

func found(name, pubkey, addr string) nodelink.Found {
	return nodelink.Found{Addr: addr, Identity: companion.Identity{Name: name, Pubkey: pubkey}}
}

func TestPickNode(t *testing.T) {
	old := found("Old", strings.Repeat("a", 64), "10.0.0.5:5000")
	repl := found("New", strings.Repeat("b", 64), "10.0.0.6:5000")
	third := found("Neighbour", strings.Repeat("c", 64), "10.0.0.7:5000")
	none := bufio.NewReader(strings.NewReader(""))

	if _, err := pickNode(nil, old.Pubkey, control.NodeDoc{}, false, none); err == nil || !strings.Contains(err.Error(), "-host") {
		t.Fatalf("nothing found: %v", err)
	}
	// One node found: that's it (the caller handles it being the current one).
	if got, err := pickNode([]nodelink.Found{repl}, old.Pubkey, control.NodeDoc{}, false, none); err != nil || got.Name != "New" {
		t.Fatalf("one: %+v %v", got, err)
	}
	// Not interactive: the only node that isn't the current one.
	if got, err := pickNode([]nodelink.Found{old, repl}, old.Pubkey, control.NodeDoc{}, false, none); err != nil || got.Name != "New" {
		t.Fatalf("old and new: %+v %v", got, err)
	}
	// Not interactive, two candidates: ambiguous, listed.
	_, err := pickNode([]nodelink.Found{old, repl, third}, old.Pubkey, control.NodeDoc{}, false, none)
	if err == nil || !strings.Contains(err.Error(), "found 3 nodes") || !strings.Contains(err.Error(), "Neighbour") {
		t.Fatalf("ambiguous: %v", err)
	}
	// Interactive: asks until it gets a number in range.
	in := bufio.NewReader(strings.NewReader("9\nx\n3\n"))
	if got, err := pickNode([]nodelink.Found{old, repl, third}, old.Pubkey, control.NodeDoc{}, true, in); err != nil || got.Name != "Neighbour" {
		t.Fatalf("interactive: %+v %v", got, err)
	}
	if _, err := pickNode([]nodelink.Found{old, repl}, old.Pubkey, control.NodeDoc{}, true, none); err == nil {
		t.Fatal("picked with no answer")
	}
}

func TestNodeWithKey(t *testing.T) {
	if got := nodeWithKey("Ridge", strings.Repeat("ab", 32)); got != "Ridge (abababababab…)" {
		t.Fatal(got)
	}
	if got := nodeWithKey("", "abc"); got != "abc" {
		t.Fatal(got)
	}
}
