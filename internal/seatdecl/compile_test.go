package seatdecl

import (
	"encoding/json"
	"strings"
	"testing"
)

func catalogue(interactive string) Classes {
	return Classes{Schema: ClassesSchema, Classes: map[string]Class{
		"seat-linux-arm64-v1": {
			Base: "base-ref", ImageRef: "seat-image", ImageDigest: "sha256:" + strings.Repeat("e", 64),
			Max: Resources{CPUs: 2, MemoryGiB: 4, DiskGiB: 16},
			Agents: map[string]Agent{"claude@2.1.280": {
				Archive: "claude-2.1.280.tar", ArchiveSHA256: strings.Repeat("f", 64),
				InteractiveQualified: interactive, RequiredHosts: []string{"api.anthropic.com", "claude.ai"},
			}},
		},
	}}
}

func parsed(t *testing.T, d Declaration) (Declaration, string) {
	t.Helper()
	p, sum, err := Parse(encode(t, d))
	if err != nil {
		t.Fatal(err)
	}
	return p, sum
}

func TestCompileMakesTheProfileFromTheCatalogueOnly(t *testing.T) {
	d, sum := parsed(t, valid())
	got, err := Compile(d, sum, catalogue("POMAR-CC SOW NN interactive run"))
	if err != nil {
		t.Fatal(err)
	}
	p := got.Profile
	if p.ImageDigest != "sha256:"+strings.Repeat("e", 64) || p.Agent != "claude" || p.AgentVersion != "2.1.280" || p.AgentArchiveSHA256 != strings.Repeat("f", 64) {
		t.Fatalf("profile %+v", p)
	}
	// The egress is the agent's required hosts plus the declaration's own,
	// each once, and nothing else.
	if strings.Join(p.AllowedHosts, ",") != "api.anthropic.com,claude.ai" {
		t.Fatalf("hosts %v", p.AllowedHosts)
	}
	if got.DeclarationSHA256 != sum || len(got.Sources) != 3 || got.Sources[0].Fork != "architect/zeocreator" || got.Sources[2].Writable {
		t.Fatalf("sources %+v", got.Sources)
	}
	// No identity, credential or fixture reaches the profile.
	b, _ := json.Marshal(got)
	for _, s := range []string{"permissions", "app_id", "qualificationFixture"} {
		if strings.Contains(string(b), `"`+s+`"`) {
			t.Fatalf("%s reached the compiled profile", s)
		}
	}
}

func TestCompileRefusesWhatTheCatalogueDoesntAdmit(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*Declaration)
		cat    Classes
	}{
		"an agent not qualified interactively": {func(*Declaration) {}, catalogue("")},
		"an unknown class":                     {func(d *Declaration) { d.Class = "seat-linux-arm64-v9" }, catalogue("q")},
		"an unpinned agent version":            {func(d *Declaration) { d.Provider.Version = "2.1.295" }, catalogue("q")},
		"too many CPUs for the class":          {func(d *Declaration) { d.Resources.CPUs = 4 }, catalogue("q")},
		"too much memory for the class":        {func(d *Declaration) { d.Resources.MemoryGiB = 6 }, catalogue("q")},
		"too much disk for the class":          {func(d *Declaration) { d.Resources.DiskGiB = 32 }, catalogue("q")},
	} {
		d := valid()
		c.mutate(&d)
		p, sum := parsed(t, d)
		if _, err := Compile(p, sum, c.cat); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	d, _ := parsed(t, valid())
	if _, err := Compile(d, "not-a-sum", catalogue("q")); err == nil {
		t.Error("compiled without the declaration's sha256")
	}
}

func TestParseClassesRefusesAnIncompleteCatalogue(t *testing.T) {
	good, _ := json.Marshal(catalogue("q"))
	if _, err := ParseClasses(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Classes){
		"another schema": func(c *Classes) { c.Schema = "x" },
		"no classes":     func(c *Classes) { c.Classes = nil },
		"an unpinned image": func(c *Classes) {
			cl := c.Classes["seat-linux-arm64-v1"]
			cl.ImageDigest = "latest"
			c.Classes["seat-linux-arm64-v1"] = cl
		},
		"no maximum": func(c *Classes) {
			cl := c.Classes["seat-linux-arm64-v1"]
			cl.Max = Resources{}
			c.Classes["seat-linux-arm64-v1"] = cl
		},
		"an unpinned archive": func(c *Classes) {
			cl := c.Classes["seat-linux-arm64-v1"]
			a := cl.Agents["claude@2.1.280"]
			a.ArchiveSHA256 = ""
			cl.Agents["claude@2.1.280"] = a
		},
		"a wildcard host": func(c *Classes) {
			cl := c.Classes["seat-linux-arm64-v1"]
			a := cl.Agents["claude@2.1.280"]
			a.RequiredHosts = []string{"*.anthropic.com"}
			cl.Agents["claude@2.1.280"] = a
		},
		"a bad agent key": func(c *Classes) {
			cl := c.Classes["seat-linux-arm64-v1"]
			cl.Agents["claude-latest"] = cl.Agents["claude@2.1.280"]
		},
	} {
		c := catalogue("q")
		mutate(&c)
		b, _ := json.Marshal(c)
		if _, err := ParseClasses(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseClasses(append(good, []byte(`{}`)...)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := ParseClasses([]byte(strings.Replace(string(good), `"schema"`, `"key":"x","schema"`, 1))); err == nil {
		t.Error("unknown field accepted")
	}
}
