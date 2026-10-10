//go:build darwin

package serviceupgrade

import (
	"context"
	"fmt"
)

func Execute(ctx context.Context, action, bundle, sha, archive string) (any, error) {
	if action == "stage" {
		path, err := Stage(ctx, bundle, sha)
		return map[string]string{"schema": "pomar.service-upgrade-stage/v1", "stage": path, "manifest_sha256": sha}, err
	}
	n, err := LoadNative(bundle, sha)
	if err != nil {
		return nil, err
	}
	defer n.Close()
	switch action {
	case "check":
		if archive != "" {
			j, err := n.LoadRollback(archive)
			if err != nil {
				return nil, err
			}
			if err = n.Check(ctx); err != nil {
				return nil, err
			}
			return n.recoveryObservation(ctx, j)
		}
		return Inspect(ctx, n)
	case "apply":
		if archive != "" {
			return nil, fmt.Errorf("apply creates a new archive; it cannot reuse an old operation")
		}
		if err = n.identity(ctx); err != nil {
			return nil, err
		}
		if err = n.Lock(); err != nil {
			return nil, err
		}
		return Apply(ctx, n, sha)
	case "rollback":
		if archive == "" {
			return nil, fmt.Errorf("rollback requires the original -archive")
		}
		if err = n.identity(ctx); err != nil {
			return nil, err
		}
		if err = n.Lock(); err != nil {
			return nil, err
		}
		j, err := n.LoadRollback(archive)
		if err != nil {
			return nil, err
		}
		return Rollback(ctx, n, j, sha)
	default:
		return nil, fmt.Errorf("unknown upgrade operation")
	}
}
