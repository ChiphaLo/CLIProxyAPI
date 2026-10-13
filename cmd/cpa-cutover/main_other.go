//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() { fmt.Fprintln(os.Stderr, "CPA cutover requires Linux"); os.Exit(1) }
