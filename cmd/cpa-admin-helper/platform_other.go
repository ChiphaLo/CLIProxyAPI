//go:build !linux

package main

import (
	"context"
	"fmt"
)

func serve(context.Context, string, string, uint32, int, string, string, uint32) error {
	return fmt.Errorf("Linux shadow environment required")
}
