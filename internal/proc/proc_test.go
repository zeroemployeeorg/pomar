package proc

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	out := "    1     0 Thu Jan  1 01:00:10 1970   3:26.27 /sbin/launchd\n" +
		"  812   503 Wed Sep 23 21:08:10 2026   0:01.50 /opt/pomar-host helper --attempt a1 --state-dir x -- /bin/sh -c sleep 5\n\n"
	ps, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("len = %d", len(ps))
	}
	p := ps[1]
	if p.PID != 812 || p.UID != 503 || p.Start != "Wed Sep 23 21:08:10 2026" || p.CPU != 1500*time.Millisecond {
		t.Fatalf("process = %+v", p)
	}
	if p.Args != "/opt/pomar-host helper --attempt a1 --state-dir x -- /bin/sh -c sleep 5" {
		t.Fatalf("args = %q", p.Args)
	}
	if _, ok := Find(ps, 812); !ok {
		t.Fatal("Find(812) missed")
	}
	if _, ok := Find(ps, 9); ok {
		t.Fatal("Find(9) found a ghost")
	}
}

func TestParseRejectsShortLines(t *testing.T) {
	if _, err := Parse("12 0 Wed Sep 23 21:08:10 2026 0:01.00\n"); err == nil {
		t.Fatal("short line accepted")
	}
}

func TestPSListsSelf(t *testing.T) {
	ps, err := PS{}.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("no processes")
	}
}

func TestParseCPU(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:00.11":      110 * time.Millisecond,
		"3:26.27":      3*time.Minute + 26270*time.Millisecond,
		"123:04.50":    123*time.Minute + 4500*time.Millisecond,
		"1:02:03.00":   time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00.5": 49*time.Hour + 500*time.Millisecond,
	} {
		got, err := ParseCPU(in)
		if err != nil || got != want {
			t.Errorf("ParseCPU(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "12", "a:00.1", "1:2:3:4.0", "x-1:00.0", "-1:00.0"} {
		if _, err := ParseCPU(in); err == nil {
			t.Errorf("ParseCPU(%q) accepted", in)
		}
	}
}
