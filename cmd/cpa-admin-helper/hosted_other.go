//go:build !linux

package main

import (
	"context"
	"errors"
)

func serveHosted(context.Context, string) error { return errors.New("verified hosting requires Linux") }
