package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/zeroemployeeorg/pomar/internal/serviceupgrade"
	"io"
	"time"
)

const upgradeHelp = `Usage:
  pomar upgrade stage -bundle DIR -manifest-sha256 SHA256
  pomar upgrade check -bundle ROOT_STAGE -manifest-sha256 SHA256 [-archive ORIGINAL_ARCHIVE]
  pomar upgrade apply -bundle ROOT_STAGE -manifest-sha256 SHA256
  pomar upgrade rollback -bundle ROOT_STAGE -manifest-sha256 SHA256 -archive ORIGINAL_ARCHIVE

Administrator operations for an existing, explicitly bound macOS service.
check is read-only. stage creates only a private verified payload stage.
apply replaces only the two frozen binaries after an idle owner drain.
rollback restores only the manifest-bound original archive after idle checks.
They do not provision accounts, grants, keys, images, classes or a new daemon.
An explicit reviewed signing policy and retained manifest SHA256 are required.
An incomplete transaction retains its archive/journal; do not replay apply.
See docs/service-upgrade.md for prerequisites, evidence and recovery limits.
`

func upgradeCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(stdout, upgradeHelp)
		return 0
	}
	action := args[0]
	switch action {
	case "stage", "check", "apply", "rollback":
	default:
		fmt.Fprint(stderr, upgradeHelp)
		return 2
	}
	fs := flag.NewFlagSet("upgrade "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bundle := fs.String("bundle", "", "frozen payload or private root stage")
	sha := fs.String("manifest-sha256", "", "independently retained manifest digest")
	archive := fs.String("archive", "", "original product archive")
	if fs.Parse(args[1:]) != nil {
		return 2
	}
	if fs.NArg() != 0 || *bundle == "" || len(*sha) != 64 {
		fmt.Fprint(stderr, upgradeHelp)
		return 2
	}
	if (*archive != "" && (action == "stage" || action == "apply")) || (action == "rollback" && *archive == "") {
		fmt.Fprint(stderr, upgradeHelp)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := serviceupgrade.Execute(ctx, action, *bundle, *sha, *archive)
	if out != nil {
		if e := json.NewEncoder(stdout).Encode(out); e != nil {
			fmt.Fprintln(stderr, "upgrade: receipt could not be written")
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "upgrade:", err)
		return 1
	}
	return 0
}
