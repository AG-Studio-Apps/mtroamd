// Command altscreen-repro is a dev/verification harness: it reconstructs
// the current alt-screen from a raw pty-output ring dump and writes the
// resulting repaint sequence to stdout. Pipe that into the SwiftTerm /
// tmux harness to confirm the reconstruction reproduces the live screen
// (footer included). Not shipped in the daemon.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/AG-Studio-Apps/mtroamd/internal/altscreen"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: altscreen-repro <ring-file> [rows] [cols] > repaint.bin")
		os.Exit(2)
	}
	ring, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rows, cols, bottomN := 42, 45, 0
	if len(os.Args) >= 4 {
		rows, _ = strconv.Atoi(os.Args[2])
		cols, _ = strconv.Atoi(os.Args[3])
	}
	if len(os.Args) >= 5 {
		bottomN, _ = strconv.Atoi(os.Args[4]) // >0 → emit the bottom-N footer redraw instead of full repaint
	}
	var r []byte
	var faithful bool
	if bottomN > 0 {
		r, faithful = altscreen.ReconstructBottomRows(ring, rows, cols, bottomN)
	} else {
		r, faithful = altscreen.Reconstruct(ring, rows, cols)
	}
	_, _ = os.Stdout.Write(r)
	fmt.Fprintf(os.Stderr, "reconstructed %dx%d bottomN=%d from %d ring bytes -> %d bytes (faithful=%v)\n",
		rows, cols, bottomN, len(ring), len(r), faithful)
}
