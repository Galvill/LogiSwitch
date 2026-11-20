//go:build !windows

// Command logiswitch runs on Windows only.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "logiswitch runs on Windows only")
	os.Exit(1)
}
