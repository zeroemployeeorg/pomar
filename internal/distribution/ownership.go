package distribution

import (
	"fmt"
	"os"
	"syscall"
)

func ownedByCaller(st os.FileInfo) error {
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("installation directory must belong to the current user")
	}
	return nil
}
