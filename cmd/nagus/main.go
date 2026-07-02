// Command nagus is the entry point for the acquisition/watch service.
//
// This is a scaffold: it wires nothing yet. The spine (connectors -> glovebox
// sanitize -> extract/tokenize -> store -> enrich -> score -> surface) and the
// v1 adapters (land, hdd) land as follow-on tasks tracked in beads. For now it
// reports its version so the image/CI have a runnable binary.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	fmt.Fprintln(os.Stderr, "nagus "+version+": scaffold; no subcommands wired yet (see docs/design).")
}
