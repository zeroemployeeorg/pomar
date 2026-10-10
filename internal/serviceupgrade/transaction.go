// Package serviceupgrade changes only the two binaries of an existing service.
// It does not provision accounts, images, callers, keys, jobs or a new daemon.
package serviceupgrade

import (
	"context"
	"fmt"
)

type Observation struct {
	Pin            string `json:"pin"`
	History        string `json:"history_sha256"`
	KeyID          string `json:"key_id"`
	PID            int    `json:"pid"`
	Birth          string `json:"process_birth"`
	Capacity       string `json:"capacity_binding_sha256"`
	CallerHistory  string `json:"caller_history_sha256"`
	OwnerCapacity  string `json:"owner_capacity_sha256"`
	CallerCapacity string `json:"caller_capacity_sha256"`
}

type Journal struct {
	Schema      string      `json:"schema"`
	ManifestSHA string      `json:"manifest_sha256"`
	Phase       string      `json:"phase"`
	Before      Observation `json:"before"`
	Archive     string      `json:"archive"`
}

// Driver methods are narrow, checked operations, not caller-supplied commands.
// Check and Observe are read-only. Stop must recheck the named process identity.
// Replace and Restore refuse bytes outside the frozen baseline/target set.
type Driver interface {
	Check(context.Context) error
	Observe(context.Context) (Observation, error)
	Archive(context.Context, Observation) (string, error)
	Save(Journal) error
	Fence(context.Context) error
	Stop(context.Context, Observation) error
	Replace(context.Context) error
	Start(context.Context) error
	Verify(context.Context, Observation) error
	Open(context.Context) error
	Restore(context.Context, Journal) error
}

// Inspect performs no lock creation, journal write or service mutation.
func Inspect(ctx context.Context, d Driver) (Observation, error) {
	if err := d.Check(ctx); err != nil {
		return Observation{}, err
	}
	return d.Observe(ctx)
}

// Apply does not reopen admission after a failed verification. Recovery is an
// explicit product rollback using the retained archive and frozen manifest.
func Apply(ctx context.Context, d Driver, manifestSHA string) (Journal, error) {
	before, err := Inspect(ctx, d)
	if err != nil {
		return Journal{}, err
	}
	archive, err := d.Archive(ctx, before)
	if err != nil {
		return Journal{}, err
	}
	j := Journal{"pomar.service-upgrade-journal/v1", manifestSHA, "prepared", before, archive}
	step := func(phase string, act func() error) error {
		// Retain intent before its effect: a crash cannot look like no change.
		j.Phase = phase
		if err := d.Save(j); err != nil {
			return err
		}
		return act()
	}
	operations := []struct {
		phase string
		act   func() error
	}{
		{"fencing", func() error { return d.Fence(ctx) }},
		{"stopping", func() error {
			now, err := d.Observe(ctx)
			if err != nil {
				return err
			}
			if now != before {
				return fmt.Errorf("service identity, history or key changed before stop")
			}
			return d.Stop(ctx, before)
		}},
		{"installing", func() error { return d.Replace(ctx) }},
		{"starting", func() error { return d.Start(ctx) }},
		{"verifying", func() error { return d.Verify(ctx, before) }},
		{"opening", func() error { return d.Open(ctx) }},
	}
	for _, op := range operations {
		if err = step(op.phase, op.act); err != nil {
			return j, fmt.Errorf("upgrade incomplete at %s; retained archive %s; admission must be reconciled: %w", j.Phase, j.Archive, err)
		}
	}
	j.Phase = "complete"
	err = d.Save(j)
	if err != nil {
		return j, fmt.Errorf("admission reopened but completion journal failed; inspect retained archive before recovery: %w", err)
	}
	return j, err
}

func Rollback(ctx context.Context, d Driver, j Journal, manifestSHA string) (Journal, error) {
	if j.Schema != "pomar.service-upgrade-journal/v1" || j.ManifestSHA != manifestSHA || j.Archive == "" {
		return j, fmt.Errorf("rollback requires the original frozen manifest and archive")
	}
	if err := d.Check(ctx); err != nil {
		return j, err
	}
	j.Phase = "rolling-back"
	if err := d.Save(j); err != nil {
		return j, err
	}
	if err := d.Fence(ctx); err != nil {
		return j, err
	}
	// Restore must either stop a positively matched idle current manager or
	// prove the service and all its writers absent. It cannot replace live files.
	if err := d.Restore(ctx, j); err != nil {
		return j, err
	}
	if err := d.Start(ctx); err != nil {
		return j, err
	}
	if err := d.Verify(ctx, j.Before); err != nil {
		return j, err
	}
	if err := d.Open(ctx); err != nil {
		return j, err
	}
	j.Phase = "rolled-back"
	if err := d.Save(j); err != nil {
		return j, fmt.Errorf("baseline admission reopened but rollback journal failed; inspect retained archive: %w", err)
	}
	return j, nil
}
