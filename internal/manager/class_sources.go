package manager

import (
	"fmt"
	"slices"
	"strings"
)

func checkClassSources(cfg Config) error {
	for _, jc := range cfg.Classes {
		if cfg.CtlSocket != "" && len(cfg.Classes) > 1 && cfg.Mirrors != nil && len(jc.SourceMirrors) == 0 {
			return fmt.Errorf("manager: class %q needs a source mirror list on a multi-class control socket", jc.Class.Name)
		}
		seen := map[string]bool{}
		for _, name := range jc.SourceMirrors {
			if !validID.MatchString(name) || seen[name] {
				return fmt.Errorf("manager: class %q has an invalid or repeated source mirror", jc.Class.Name)
			}
			if cfg.MirrorURLs != nil && cfg.MirrorURLs[name] == "" {
				return fmt.Errorf("manager: class %q names unconfigured mirror %q", jc.Class.Name, name)
			}
			seen[name] = true
		}
		if jc.SourceRef != "" && (len(jc.SourceMirrors) == 0 || strings.ContainsAny(jc.SourceRef, "\x00\r\n")) {
			return fmt.Errorf("manager: class %q needs an explicit mirror list and a valid source ref", jc.Class.Name)
		}
		for _, arg := range jc.Command {
			if arg == "" || strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("manager: class %q has an invalid fixed command", jc.Class.Name)
			}
		}
	}
	return nil
}

func (jc JobClass) checkWorkload(command []string, src *Source) error {
	if len(jc.Command) != 0 && !slices.Equal(command, jc.Command) {
		return fmt.Errorf("manager: class %q requires its fixed command", jc.Class.Name)
	}
	if src != nil && len(jc.SourceMirrors) != 0 && !slices.Contains(jc.SourceMirrors, src.Mirror) {
		return fmt.Errorf("manager: source mirror %q is not allowed for class %q", src.Mirror, jc.Class.Name)
	}
	if jc.SourceRef != "" && (src == nil || src.Ref != jc.SourceRef || !fullSHA.MatchString(src.SHA)) {
		return fmt.Errorf("manager: class %q requires ref %q and an explicit full source SHA", jc.Class.Name, jc.SourceRef)
	}
	return nil
}
