package seatdecl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// ClassesSchema names the owner's seat class catalogue.
const ClassesSchema = "pomar.seat-classes/v1"

// Classes is the owner-supplied catalogue a declaration is compiled
// against. A declaration names a class and an agent version; only the owner
// says what those mean (an image, its bounds, a pinned agent package) and
// which agent versions are qualified for an interactive seat.
type Classes struct {
	Schema  string           `json:"schema"`
	Classes map[string]Class `json:"classes"`
}

type Class struct {
	Base        string           `json:"base"`
	ImageRef    string           `json:"image_ref"`
	ImageDigest string           `json:"image_digest"`
	Max         Resources        `json:"max"`
	Agents      map[string]Agent `json:"agents"` // key: "<agent>@<version>"
}

// Agent is one pinned agent package in a class. A seat runs only an agent
// whose interactive qualification is recorded (SOW 15 §2.3, §5.4); the
// headless qualification alone doesn't admit a seat.
type Agent struct {
	Archive              string   `json:"archive"`
	ArchiveSHA256        string   `json:"archive_sha256"`
	InteractiveQualified string   `json:"interactive_qualified"` // the qualification record; empty means not qualified
	RequiredHosts        []string `json:"required_hosts"`
}

// Binding is one source as the environment will receive it: the declared
// pins and destination, matched later to the owner's verified source
// artifact (SOW 16 §2). It carries no GitHub authority.
type Binding struct {
	Role        string `json:"role"`
	Repository  string `json:"repository"`
	Fork        string `json:"fork,omitempty"`
	Commit      string `json:"commit"`
	Tree        string `json:"tree"`
	Destination string `json:"destination"`
	Writable    bool   `json:"writable"`
}

// Compiled is a declaration made concrete for one class: the environment
// profile, the ordered source bindings, and the declaration's identity.
type Compiled struct {
	DeclarationSHA256 string                      `json:"declaration_sha256"`
	Profile           agentenv.EnvironmentProfile `json:"profile"`
	Sources           []Binding                   `json:"sources"`
	Resources         Resources                   `json:"resources"`
}

var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseClasses reads the owner's catalogue from exactly its bytes.
func ParseClasses(raw []byte) (Classes, error) {
	if len(raw) == 0 || len(raw) > 256<<10 {
		return Classes{}, errors.New("a class catalogue is 1 byte to 256 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Classes
	if err := dec.Decode(&c); err != nil {
		return Classes{}, fmt.Errorf("class catalogue: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Classes{}, errors.New("class catalogue: trailing data")
	}
	if c.Schema != ClassesSchema || len(c.Classes) == 0 {
		return Classes{}, fmt.Errorf("class catalogue schema must be %s, with at least one class", ClassesSchema)
	}
	for name, cl := range c.Classes {
		if !className.MatchString(name) || cl.Base == "" || cl.ImageRef == "" || !imageDigest.MatchString(cl.ImageDigest) {
			return Classes{}, fmt.Errorf("class %q needs a base and a digest-pinned image", name)
		}
		if cl.Max.CPUs < 1 || cl.Max.MemoryGiB < 1 || cl.Max.DiskGiB < 1 {
			return Classes{}, fmt.Errorf("class %q needs its maximum resources", name)
		}
		for key, a := range cl.Agents {
			if !agentKey.MatchString(key) || a.Archive == "" || !digest.MatchString(a.ArchiveSHA256) {
				return Classes{}, fmt.Errorf("class %q agent %q needs agent@version and a pinned archive", name, key)
			}
			if err := validateHosts(a.RequiredHosts); err != nil {
				return Classes{}, err
			}
		}
	}
	return c, nil
}

var agentKey = regexp.MustCompile(`^(claude|codex)@[0-9]+(\.[0-9]+){0,3}$`)

// Compile makes a parsed declaration concrete against the owner's
// catalogue, refusing anything the catalogue doesn't admit. It doesn't
// install the profile, start anything or contact anyone.
func Compile(d Declaration, sum string, c Classes) (Compiled, error) {
	if err := d.Validate(); err != nil {
		return Compiled{}, err
	}
	if !digest.MatchString(sum) {
		return Compiled{}, errors.New("compile needs the declaration's sha256 from Parse")
	}
	cl, ok := c.Classes[d.Class]
	if !ok {
		return Compiled{}, fmt.Errorf("class %q isn't in the owner's catalogue", d.Class)
	}
	r := d.Resources
	if r.CPUs > cl.Max.CPUs || r.MemoryGiB > cl.Max.MemoryGiB || r.DiskGiB > cl.Max.DiskGiB {
		return Compiled{}, fmt.Errorf("resources exceed class %q's maximum", d.Class)
	}
	key := d.Provider.Agent + "@" + d.Provider.Version
	a, ok := cl.Agents[key]
	if !ok {
		return Compiled{}, fmt.Errorf("%s isn't pinned in class %q", key, d.Class)
	}
	if a.InteractiveQualified == "" {
		return Compiled{}, fmt.Errorf("%s isn't qualified for an interactive seat; its qualification comes first (SOW 15 §5.4)", key)
	}
	hosts := slices.Clone(a.RequiredHosts)
	for _, h := range d.Network.AllowedHosts {
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	slices.Sort(hosts)
	p := agentenv.EnvironmentProfile{
		Base: cl.Base, ImageRef: cl.ImageRef, ImageDigest: cl.ImageDigest,
		AllowedHosts: hosts, Agent: d.Provider.Agent, AgentVersion: d.Provider.Version,
		AgentArchive: a.Archive, AgentArchiveSHA256: a.ArchiveSHA256,
	}
	if p.Agent == "codex" {
		p.Agent, p.AgentVersion = "codex", ""
	}
	var sources []Binding
	for _, s := range d.Sources {
		sources = append(sources, Binding{Role: s.Role, Repository: s.Repository, Fork: s.Fork, Commit: s.Commit, Tree: s.Tree, Destination: s.Destination, Writable: s.Writable})
	}
	return Compiled{DeclarationSHA256: sum, Profile: p, Sources: sources, Resources: r}, nil
}
