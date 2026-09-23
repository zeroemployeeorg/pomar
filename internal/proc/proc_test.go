package proc

import "testing"

func TestParse(t *testing.T) {
	out := "    1     0 Thu Jan  1 01:00:10 1970     /sbin/launchd\n" +
		"  812   503 Wed Sep 23 21:08:10 2026     /opt/pomar-host helper --attempt a1 --state-dir x -- /bin/sh -c sleep 5\n\n"
	ps, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("len = %d", len(ps))
	}
	p := ps[1]
	if p.PID != 812 || p.UID != 503 || p.Start != "Wed Sep 23 21:08:10 2026" {
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
	if _, err := Parse("12 0 Wed Sep\n"); err == nil {
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
