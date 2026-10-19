// loopbackcontrol pins CI to a responding configured resolver, never a public
// default. A named address argument is proved alone and never falls back.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: loopbackcontrol [address]")
		os.Exit(2)
	}
	caller := ""
	if len(os.Args) == 2 {
		caller = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	target, reason := sandboxprobe.DiscoverAndProveControl(ctx, caller)
	if reason != "" {
		fmt.Fprintln(os.Stderr, reason)
		os.Exit(1)
	}
	fmt.Println(target.Addr)
}
