//go:build !darwin && !linux

package hostpolicy

import "os"

func kernelACLSafe(*os.File) bool { return false }
