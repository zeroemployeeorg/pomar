package agentenv

import (
	"context"
	"errors"
	"path"
	"strings"
)

var errCredentialExport = errors.New("result export refused: a credential or private configuration path changed")

// Credential stores and private configuration are never result artifacts.
// This is a path policy, not secret discovery: credentials copied into ordinary
// source files can still be exported. Controllers must treat results as private,
// untrusted workspace data, not safe-to-publish output.
func credentialExportPath(name string) bool {
	for _, component := range strings.Split(name, "/") {
		switch component {
		case ".codex", ".claude", ".ssh", ".aws", ".azure", ".kube":
			return true
		}
	}
	base := path.Base(name)
	switch base {
	case ".credentials.json", ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519", ".env":
		return true
	}
	return strings.HasPrefix(base, ".env.") && base != ".env.example" && base != ".env.sample" && base != ".env.template"
}

func checkExportPaths(raw []byte) error {
	for _, name := range strings.Split(string(raw), "\x00") {
		if name != "" && credentialExportPath(name) {
			return errCredentialExport
		}
	}
	return nil
}

// Export only the literal paths inspected above. A new sensitive path created
// between listing and diff generation cannot join an unrestricted Git diff.
func (b *Broker) exportDiff(ctx context.Context) ([]byte, error) {
	names, err := b.gitOutput(ctx, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	if err := checkExportPaths(names); err != nil {
		return nil, err
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "HEAD", "--"}
	for _, name := range strings.Split(string(names), "\x00") {
		if name != "" {
			args = append(args, ":(literal)"+name)
		}
	}
	if len(args) == 7 {
		return nil, nil
	}
	return b.gitOutput(ctx, args...)
}
