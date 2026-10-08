//go:build !darwin

package calljournal

import "os"

func flush(f *os.File) error { return f.Sync() }
