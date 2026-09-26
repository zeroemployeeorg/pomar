package manager

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

func TestAccessArgs(t *testing.T) {
	src := &Source{Mirror: "example", Ref: "main"}
	ro := &Source{Mirror: "example", Ref: "main", ReadOnly: true}
	for name, tc := range map[string]struct {
		src  *Source
		jc   JobClass
		want []string
	}{
		"a writable source":                        {src, JobClass{}, nil},
		"a read-only source":                       {ro, JobClass{}, []string{"--readonly-source", "yes"}},
		"a read-only source in a job-user class":   {ro, JobClass{JobUser: true}, []string{"--readonly-source", "yes"}},
		"a source in a job-user class":             {src, JobClass{JobUser: true}, nil}, // a source always runs as the job user
		"no source":                                {nil, JobClass{}, nil},
		"no source in a class that needs the user": {nil, JobClass{JobUser: true}, []string{"--job-user", "yes"}},
	} {
		if got := accessArgs(tc.src, tc.jc); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestDefaultClassTakesJobUser(t *testing.T) {
	v, err := venue.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	m, err := Open(Config{Venue: v, HostBin: self, Procs: noProcs{}, UID: os.Getuid(), Poll: time.Hour,
		Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}, JobUser: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if jc, _ := m.jobClass(""); !jc.JobUser {
		t.Fatal("the default class does not require the job user")
	}
}

func TestResultRecordsReadOnlySourceAndJobUser(t *testing.T) {
	m := &Manager{}
	e := terminalEntry("ro")
	if _, ok := m.resultDoc(e)["readonly_source"]; ok {
		t.Fatal("readonly_source on a writable source")
	}
	e.Source.ReadOnly = true
	if m.resultDoc(e)["readonly_source"] != true {
		t.Fatal("readonly_source missing")
	}
	e = terminalEntry("ju")
	e.Source, e.GoProxy, e.JobUser = nil, false, true
	env := m.resultDoc(e)["env"].(map[string]any)
	if env["uid"] != jobUID || env["HOME"] != jobHomeWithoutSource {
		t.Fatalf("job-user env = %v", env)
	}
	e.JobUser = false
	if env := m.resultDoc(e)["env"].(map[string]any); len(env) != 0 {
		t.Fatalf("a root start with no source claims a job user: %v", env)
	}
}
