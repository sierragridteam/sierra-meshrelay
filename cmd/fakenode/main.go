// Command fakenode runs a fake MeshCore companion node on TCP, for driving
// meshbridge without hardware. It answers APP_START and DEVICE_QUERY, and
// echoes every other frame back with a 0x80 prefix.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/sierragridteam/sierra-meshrelay/internal/fakenode"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:5000", "address to listen on")
	name := flag.String("name", "FakeNode", "node name (also seeds the pubkey)")
	model := flag.String("model", "Fake Board", "board model reported by DEVICE_INFO")
	flag.Parse()

	n, err := fakenode.Listen(*addr, fakenode.Options{Name: *name, Model: *model})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakenode:", err)
		os.Exit(1)
	}
	fmt.Printf("fake node %q listening on %s\npubkey %s\n", *name, n.Addr(), n.PubkeyHex())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	n.Close()
}
