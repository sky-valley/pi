// Command pi-env is the pi-env daemon: `pi-env serve --token <hex>` speaks
// the protocol of docs/protocol.md on stdin and stdout.
package main

import (
	"os"

	"github.com/sky-valley/pi/env/daemon"
)

func main() {
	os.Exit(daemon.Main(os.Args))
}
