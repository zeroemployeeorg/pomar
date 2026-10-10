package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUpgradeHelpAndMissingBindingNeverOperate(t *testing.T) {
	t.Setenv("POMAR_DATA_ROOT", "/private/var/pomar/data")
	t.Setenv("POMAR_SOCKET", "/private/var/pomar/run/ctl.sock")
	for _, a := range [][]string{{"upgrade", "help"}, {"help", "upgrade"}, {"upgrade", "check"}, {"upgrade", "apply", "-bundle", "/not/a/real/payload"}, {"upgrade", "rollback"}} {
		var out, err bytes.Buffer
		rc := run(a, &out, &err)
		if a[len(a)-1] == "help" || a[0] == "help" {
			if rc != 0 || !strings.Contains(out.String(), "read-only") {
				t.Fatal(a, rc, out.String(), err.String())
			}
		} else if rc != 2 {
			t.Fatal(a, rc, err.String())
		}
	}
}
