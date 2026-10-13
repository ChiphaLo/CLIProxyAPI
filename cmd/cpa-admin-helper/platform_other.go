//go:build !linux

package main

import (
	"context"
	"fmt"
)

func serve(context.Context, string, string, uint32, int) error {
	return fmt.Errorf("Linux shadow environment required")
}
