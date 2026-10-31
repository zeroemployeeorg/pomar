package agentenv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// HarnessSwitch is retained with the original replacement operation. It binds
// the selected owner profile before stopping anything, without copying secrets
// or claiming one provider can resume another provider's native conversation.
type HarnessSwitch struct {
	SessionID     string `json:"session_id"`
	FromProfile   string `json:"from_profile"`
	TargetProfile string `json:"target_profile"`
	FromAgent     string `json:"from_agent"`
	TargetAgent   string `json:"target_agent"`
	TargetBinding string `json:"target_binding"`
	Phase         string `json:"phase"` // prepared, selected; Action carries execution outcome
}

func harnessName(c HostConfig) string {
	if c.Agent == "" {
		return "codex"
	}
	return c.Agent
}

// prepareHarnessSwitch does no mutation. A caller names only an installed owner
// profile, never a binary, secret, policy override or filesystem location.
func (h *Host) prepareHarnessSwitch(e *Environment, profile, session string) (HostConfig, *HarnessSwitch, error) {
	if session == "" || session != e.Spec.Session {
		return HostConfig{}, nil, ErrStale
	}
	if e.Phase != "running" && e.Phase != "stopped" && e.Phase != "fenced" {
		return HostConfig{}, nil, errors.New("harness switch requires an established seat")
	}
	if profile == "" || profile == e.Spec.Profile {
		return HostConfig{}, nil, errors.New("name a different owner harness profile")
	}
	c, err := h.profileConfig(profile)
	if err != nil {
		return HostConfig{}, nil, err
	}
	old := e.Spec.HostConfig
	if old.Seat == "" || c.Seat != old.Seat || old.QualificationFixture != nil || c.QualificationFixture != nil || len(c.ControllerCapabilities) != 0 {
		return HostConfig{}, nil, errors.New("harness switch requires two interactive profiles for the same seat")
	}
	if harnessName(old) == harnessName(c) {
		return HostConfig{}, nil, errors.New("target must select a different harness")
	}
	if err = ValidateAgent(c); err != nil {
		return HostConfig{}, nil, err
	}
	if harnessName(c) == "codex" {
		if err = c.SeatLaunch.Validate(); err != nil {
			return HostConfig{}, nil, err
		}
	} else if c.SeatLaunch != nil {
		return HostConfig{}, nil, errors.New("Claude profile cannot carry Codex launch settings")
	}
	hex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !filepath.IsAbs(c.CodexArchive) || !hex.MatchString(c.CodexArchiveSHA256) || !hex.MatchString(c.SeatDeclarationSHA256) {
		return HostConfig{}, nil, errors.New("target needs pinned archive and seat declaration")
	}
	// Switching harness must not become an implicit image/project/host migration.
	if old.Root != c.Root || old.Store != c.Store || old.Helper != c.Helper || old.Base != c.Base || old.Kernel != c.Kernel || old.InitRef != c.InitRef || old.InitDigest != c.InitDigest || old.ImageRef != c.ImageRef || old.ImageDigest != c.ImageDigest || old.SourceSHA != c.SourceSHA || old.SourceBundle != c.SourceBundle || old.GuestBinary != c.GuestBinary || old.ShimBinary != c.ShimBinary || old.CPUs != c.CPUs || old.MemoryBytes != c.MemoryBytes {
		return HostConfig{}, nil, errors.New("target changes retained box inputs or resources; harness switch refused")
	}
	if err = verifyHarnessArchive(c); err != nil {
		return HostConfig{}, nil, err
	}
	binding := harnessBinding(c)
	return c, &HarnessSwitch{SessionID: session, FromProfile: e.Spec.Profile, TargetProfile: profile, FromAgent: harnessName(old), TargetAgent: harnessName(c), TargetBinding: binding, Phase: "prepared"}, nil
}

// selectHarness is called only after stop and predecessor evidence succeed.
// Selection and its receipt share the same durable environment journal write.
func (h *Host) selectHarness(e *Environment, operation string, c HostConfig) error {
	a := e.Actions[operation]
	if a.Switch == nil || a.Switch.Phase != "prepared" || a.Switch.FromProfile != e.Spec.Profile || a.Switch.FromAgent != harnessName(e.Spec.HostConfig) || a.Switch.SessionID != e.Spec.Session || a.Expected != e.Spec.Incarnation || a.Switch.TargetBinding != harnessBinding(c) {
		return ErrConflict
	}
	if (e.Phase != "stopped" && e.Phase != "fenced") || e.LastFence == nil || !e.LastFence.TerminationConfirmed || !e.LastFence.WorkspaceWriteRevoked || !e.LastFence.NetworkRevoked || !e.LastFence.AdapterAccessRevoked || e.LastFence.Incarnation != e.Spec.Incarnation {
		return errors.New("predecessor is not fenced")
	}
	if _, err := h.ensurePredecessor(e); err != nil {
		return err
	}
	e.Spec.HostConfig = c
	e.Spec.Profile = a.Switch.TargetProfile
	e.Spec.RequireRetainedDisk = true
	copySwitch := *a.Switch
	copySwitch.Phase = "selected"
	a.Switch = &copySwitch
	e.Actions[operation] = a
	return h.save(e)
}

func harnessBinding(c HostConfig) string {
	b, _ := json.Marshal(struct {
		Agent, Version, Archive, ArchiveSHA, Seat, Declaration string
		Launch                                                 *SeatLaunch
		Hosts                                                  []string
	}{harnessName(c), c.AgentVersion, c.CodexArchive, c.CodexArchiveSHA256, c.Seat, c.SeatDeclarationSHA256, c.SeatLaunch, c.AllowedHosts})
	return digest(b)
}
func verifyHarnessArchive(c HostConfig) error {
	f, e := os.Open(c.CodexArchive)
	if e != nil {
		return errors.New("target harness archive unavailable")
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Size() > 2<<30 {
		return errors.New("target harness archive is not a bounded regular file")
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, (2<<30)+1))
	if e != nil || n != info.Size() || hex.EncodeToString(h.Sum(nil)) != c.CodexArchiveSHA256 {
		return errors.New("target harness archive digest mismatch")
	}
	return nil
}

func sameHarnessRequest(s *HarnessSwitch, profile, session string) bool {
	if s == nil {
		return profile == "" && session == ""
	}
	return s.TargetProfile == profile && s.SessionID == session
}

func hasHarnessSwitch(e *Environment) bool {
	for _, a := range e.Actions {
		if a.Switch != nil && a.Switch.Phase == "selected" {
			return true
		}
	}
	return false
}
