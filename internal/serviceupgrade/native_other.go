//go:build !darwin

package serviceupgrade

import (
	"context"
	"fmt"
)

func PlatformAvailable() error                              { return fmt.Errorf("existing-service upgrade supports macOS only") }
func Stage(context.Context, string, string) (string, error) { return "", PlatformAvailable() }
