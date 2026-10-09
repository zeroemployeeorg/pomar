// Package seatdecl reads a seat declaration: the owner-supplied, immutable
// statement of one seat's sources, environment, provider and identity
// (POMAR-CC SOW 15 §3 as corrected by SOW 16 §1). It is kept beside the
// environment profile and compiled into one; its sha256 is its identity.
//
// A declaration is data, never authority: it grants nothing and starts
// nothing. Its mutable companions, the seat's location (location.go) and
// its observed session state, are kept apart from it, so replacing a
// declaration never changes a live incarnation silently.
package seatdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Schema names this format.
const Schema = "pomar.seat-declaration/v1"

// Limit bounds a declaration's bytes.
const Limit = 64 << 10

// NotEstablished marks an allocation the organisation hasn't made yet: a
// seat's role or provider account. It is never inferred (SOW 16 §1.5).
const NotEstablished = "NOT_ESTABLISHED"

type Declaration struct {
	Schema    string     `json:"schema"`
	Seat      string     `json:"seat"`
	Project   string     `json:"project"`
	Sources   []Source   `json:"sources"`
	Class     string     `json:"class"`
	Resources Resources  `json:"resources"`
	Network   Network    `json:"network"`
	Provider  Provider   `json:"provider"`
	Resume    Resume     `json:"resume"`
	Identity  Identity   `json:"identity"`
	HostOnly  []HostOnly `json:"host_only"`
}

// Source is one repository the seat's workspace is built from, at a full
// pin. A writable source (the work lane, and the records lane if there is
// one) is cloned and pushed on its fork; a read-only one is owner-supplied
// data (SOW 16 §2).
type Source struct {
	Role        string `json:"role"`
	Repository  string `json:"repository"`
	Fork        string `json:"fork,omitempty"`
	Branch      string `json:"branch"`
	Commit      string `json:"commit"`
	Tree        string `json:"tree"`
	Destination string `json:"destination"`
	Writable    bool   `json:"writable"`
	Locks       []Lock `json:"locks,omitempty"`
}

type Lock struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Resources struct {
	CPUs      int `json:"cpus"`
	MemoryGiB int `json:"memory_gib"`
	DiskGiB   int `json:"disk_gib"`
}

type Network struct {
	AllowedHosts []string `json:"allowed_hosts"`
}

type Provider struct {
	Agent   string `json:"agent"`
	Version string `json:"version"`
	Account string `json:"account"`
}

type Resume struct {
	Policy      string `json:"policy"`
	HandoverSOW string `json:"handover_sow"`
}

// Identity is the seat's R-36 identity: its role's App on the architect
// account's forks. An unbound role carries no App (SOW 16 §8).
type Identity struct {
	Role           string            `json:"role"`
	AppID          int64             `json:"app_id,omitempty"`
	InstallationID int64             `json:"installation_id,omitempty"`
	Forks          []string          `json:"forks"`
	Push           []string          `json:"push"`
	Permissions    map[string]string `json:"permissions"`
	GitAuthor      string            `json:"git_author,omitempty"`
}

type HostOnly struct {
	What  string `json:"what"`
	Route string `json:"route"`
}

var (
	seatName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	repository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	objectID   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digest     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	version    = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}$`)
	label      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	className  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	branchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// roles are R-36c's adopted role list. Nothing is added from a seat's other
// responsibilities: a new role needs its adoption first.
var roles = map[string]bool{"master": true, "principal": true, "sparring": true, "steward": true, NotEstablished: true}
var resumePolicies = map[string]bool{"fresh-from-sow": true, "import-transcript": true}
var hostRoutes = map[string]bool{"host-act": true, "stays-on-host": true, "after-p6": true}

// Parse reads one declaration from exactly its bytes, refusing unknown
// fields, a key given twice (case-folded), trailing data and anything
// Validate refuses. It returns the declaration and its sha256, the
// declaration's identity.
func Parse(raw []byte) (Declaration, string, error) {
	if len(raw) == 0 || len(raw) > Limit {
		return Declaration{}, "", errors.New("a declaration is 1 byte to 64 KiB")
	}
	var d Declaration
	if err := strictDecode(raw, &d, "declaration"); err != nil {
		return Declaration{}, "", err
	}
	if err := d.Validate(); err != nil {
		return Declaration{}, "", err
	}
	sum := sha256.Sum256(raw)
	return d, hex.EncodeToString(sum[:]), nil
}

// Validate refuses a declaration that is malformed or that asks for more
// than R-36 and SOW 16 allow. It never consults the network or GitHub.
func (d Declaration) Validate() error {
	if d.Schema != Schema {
		return fmt.Errorf("schema must be %s", Schema)
	}
	if !seatName.MatchString(d.Seat) || !seatName.MatchString(d.Project) {
		return errors.New("seat and project are lower-case names")
	}
	if err := d.validateSources(); err != nil {
		return err
	}
	if !className.MatchString(d.Class) {
		return errors.New("class is a catalogue name")
	}
	r := d.Resources
	if r.CPUs < 1 || r.CPUs > 8 || r.MemoryGiB < 1 || r.MemoryGiB > 32 || r.DiskGiB < 1 || r.DiskGiB > 256 {
		return errors.New("resources are out of bounds (1-8 CPUs, 1-32 GiB memory, 1-256 GiB disk)")
	}
	if err := validateHosts(d.Network.AllowedHosts); err != nil {
		return err
	}
	p := d.Provider
	if (p.Agent != "claude" && p.Agent != "codex") || !version.MatchString(p.Version) {
		return errors.New("provider is claude or codex, at a pinned numeric version")
	}
	// A label names an allocation; it is short, and never shaped like a
	// provider key (sk-...). GitHub token prefixes carry "_", which a label
	// can't.
	if p.Account != NotEstablished && (!label.MatchString(p.Account) || strings.HasPrefix(p.Account, "sk-")) {
		return errors.New("provider account is a label, or NOT_ESTABLISHED; never a credential")
	}
	if !resumePolicies[d.Resume.Policy] || strings.TrimSpace(d.Resume.HandoverSOW) == "" {
		return errors.New("resume needs a known policy and its handover SOW")
	}
	if err := d.validateIdentity(); err != nil {
		return err
	}
	for _, h := range d.HostOnly {
		if strings.TrimSpace(h.What) == "" || !hostRoutes[h.Route] {
			return errors.New("each host-only case names what, and a route: host-act, stays-on-host or after-p6")
		}
	}
	return nil
}

func (d Declaration) validateSources() error {
	if len(d.Sources) == 0 || len(d.Sources) > 32 {
		return errors.New("a seat has 1 to 32 sources")
	}
	count := map[string]int{}
	var dests []string
	for _, s := range d.Sources {
		count[s.Role]++
		switch s.Role {
		case "work", "records":
			if !s.Writable || !repository.MatchString(s.Fork) || strings.EqualFold(s.Fork, s.Repository) {
				return fmt.Errorf("the %s source is writable, on a fork distinct from its golden repository", s.Role)
			}
		case "read_only":
			if s.Writable || s.Fork != "" {
				return errors.New("a read-only source has no fork and isn't writable")
			}
		default:
			return fmt.Errorf("unknown source role %q", s.Role)
		}
		if !repository.MatchString(s.Repository) || !branchName.MatchString(s.Branch) || strings.Contains(s.Branch, "..") {
			return errors.New("a source names its owner/repository and branch")
		}
		if !objectID.MatchString(s.Commit) || !objectID.MatchString(s.Tree) {
			return errors.New("every source is pinned to a full commit and tree")
		}
		dest := s.Destination
		if !innerPath(dest) {
			return fmt.Errorf("destination %q must be a clean relative path inside the workspace", dest)
		}
		for _, l := range s.Locks {
			if !innerPath(l.Path) || !digest.MatchString(l.SHA256) {
				return errors.New("a lock names a clean relative path and its sha256")
			}
		}
		dests = append(dests, dest)
	}
	// A records source is optional: under org-memory DESIGN-01 a seat submits
	// its records through Messenger rather than carrying a records lane.
	if count["work"] != 1 || count["records"] > 1 {
		return errors.New("a seat has exactly one work source and at most one records source")
	}
	for i, a := range dests {
		for _, b := range dests[i+1:] {
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return fmt.Errorf("destinations %q and %q overlap", a, b)
			}
		}
	}
	return nil
}

func (d Declaration) validateIdentity() error {
	id := d.Identity
	if !roles[id.Role] {
		return fmt.Errorf("unknown role %q", id.Role)
	}
	if id.Role == NotEstablished {
		if id.AppID != 0 || id.InstallationID != 0 {
			return errors.New("an unbound role carries no App")
		}
	} else if id.AppID <= 0 || id.InstallationID <= 0 {
		return errors.New("an established role names its App and installation")
	}
	var forks []string
	golden := map[string]bool{}
	for _, s := range d.Sources {
		golden[strings.ToLower(s.Repository)] = true
		if s.Writable {
			forks = append(forks, s.Fork)
		}
	}
	got := slices.Clone(id.Forks)
	slices.Sort(got)
	slices.Sort(forks)
	if !slices.Equal(got, forks) {
		return errors.New("identity forks must be exactly the writable sources' forks")
	}
	for _, f := range id.Forks {
		if golden[strings.ToLower(f)] {
			return errors.New("a golden repository is never an identity fork")
		}
	}
	if len(id.Push) == 0 {
		return errors.New("identity names the refs the seat may push")
	}
	for _, p := range id.Push {
		if !pushAllowed(d.Seat, p) {
			return fmt.Errorf("push ref %q is outside refs/heads/%s/ and refs/heads/records/%s", p, d.Seat, d.Seat)
		}
	}
	if len(id.Permissions) == 0 {
		return errors.New("identity names its permissions")
	}
	for name, level := range id.Permissions {
		switch name {
		case "contents":
			if level != "read" && level != "write" {
				return errors.New("contents is read or write")
			}
		case "metadata":
			if level != "read" {
				return errors.New("metadata is read")
			}
		case "workflows":
			if level != "write" || id.Role != "steward" {
				return errors.New("workflows is for the steward only (R-36c)")
			}
		default:
			return fmt.Errorf("permission %q is never granted to a seat (R-36c)", name)
		}
	}
	if id.Role != NotEstablished && !strings.HasSuffix(strings.SplitN(id.GitAuthor, " ", 2)[0], "[bot]") {
		return errors.New("an established identity commits as its App's bot")
	}
	return nil
}

// pushAllowed admits only the seat's own branches, as an exact ref or a
// pattern ending in "/*": refs/heads/<seat>/..., and its records lane,
// refs/heads/records/<seat> or below it. Never the default branch, a tag, or
// another seat's lane. This is policy, which the fork's rulesets enforce
// (POMAR-CC SOW 17 §1), not a boundary.
func pushAllowed(seat, ref string) bool {
	name := strings.TrimSuffix(ref, "/*")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "//") || strings.ContainsAny(name, "*?[ ") || strings.HasSuffix(name, "/") {
		return false
	}
	own, lane := "refs/heads/"+seat+"/", "refs/heads/records/"+seat
	switch {
	case name == lane, strings.HasPrefix(name, lane+"/"):
		return true
	case strings.HasPrefix(name+"/", own) && name+"/" != own:
		return true
	case ref == strings.TrimSuffix(own, "/")+"/*":
		return true
	}
	return false
}

// dnsName is a fully spelled lower-case DNS name: two or more labels of
// letters, digits and inner hyphens, at most 253 characters, no trailing
// dot. Anything else, whitespace and controls included, is refused here,
// at the declaration's own boundary; the egress check stays independent.
var dnsName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validateHosts(hosts []string) error {
	seen := map[string]bool{}
	for _, h := range hosts {
		if len(h) > 253 || !dnsName.MatchString(h) || net.ParseIP(h) != nil || seen[h] {
			return fmt.Errorf("allowed host %q must be an exact, unique, lower-case DNS name", h)
		}
		seen[h] = true
	}
	return nil
}

// innerPath admits a clean relative path that names something inside its
// root: never empty, absolute, ".", "..", or reaching above the root, and
// never carrying a control character or NUL.
func innerPath(p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
