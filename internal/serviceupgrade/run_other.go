//go:build !darwin

package serviceupgrade

import "context"

func Execute(context.Context, string, string, string, string) (any, error) {
	return nil, PlatformAvailable()
}
