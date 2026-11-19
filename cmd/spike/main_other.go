//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("Windows only")
	os.Exit(1)
}
