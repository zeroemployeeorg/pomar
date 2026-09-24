package capacity

import (
	"errors"
	"testing"
)

func reason(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func TestHostFromReservesForTheHost(t *testing.T) {
	h := HostFrom(16, 48*GiB)
	if h.CPUSlots != 14 || h.MemoryBytes != 40*GiB {
		t.Fatalf("HostFrom(16, 48 GiB) = %+v", h)
	}
	if h := HostFrom(1, GiB); h.CPUSlots != 0 || h.MemoryBytes != 0 {
		t.Fatalf("HostFrom(1, 1 GiB) = %+v, want zero slots", h)
	}
}

func TestAdmit(t *testing.T) {
	c := Class{Name: "t", VCPU: 2, MemoryBytes: 4 * GiB, DiskPeakBytes: 2 * GiB}
	big := Disk{Used: 0, Avail: 1000 * GiB}
	host := Host{CPUSlots: 6, MemoryBytes: 64 * GiB}
	for _, tc := range []struct {
		name string
		live []Class
		h    Host
		d    Disk
		want string
	}{
		{"fits", nil, host, big, ""},
		{"cpu slots full", []Class{c, c, c}, host, big, ReasonCPU},
		{"cpu exactly full admits", []Class{c, c}, host, big, ""},
		{"memory slots full", []Class{c}, Host{CPUSlots: 16, MemoryBytes: 7 * GiB}, big, ReasonMemory},
		// 12 GiB per claim: two live plus this one need 36 GiB.
		{"disk peak", []Class{c, c}, host, Disk{Avail: 35 * GiB}, ReasonDisk},
		{"disk exactly enough", []Class{c, c}, host, Disk{Avail: 36 * GiB}, ""},
		// 100 GiB container, 70 used: 12 more is 82%, 24 more is 94%.
		{"floor holds", nil, host, Disk{Used: 70 * GiB, Avail: 30 * GiB, Shared: true}, ""},
		{"floor refuses", []Class{c}, host, Disk{Used: 70 * GiB, Avail: 30 * GiB, Shared: true}, ReasonFloor},
		{"no floor on a dedicated volume", []Class{c}, host, Disk{Used: 70 * GiB, Avail: 30 * GiB}, ""},
	} {
		if got := reason(Admit(c, tc.live, tc.h, tc.d)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRefusalIsErrRefused(t *testing.T) {
	err := Admit(CI, nil, Host{}, Disk{Avail: 100 * GiB})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Admit on a host with no slots = %v, want ErrRefused", err)
	}
}

func TestAdmitRejectsInvalidClass(t *testing.T) {
	if err := Admit(Class{Name: "x"}, nil, Host{CPUSlots: 8, MemoryBytes: 8 * GiB}, Disk{Avail: 100 * GiB}); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("Admit(zero class) = %v, want a validation error", err)
	}
}

func TestFitsIsTheMinimumOfTheThree(t *testing.T) {
	c := Class{Name: "t", VCPU: 2, MemoryBytes: 4 * GiB, DiskPeakBytes: 2 * GiB}
	if n := Fits(c, Host{CPUSlots: 14, MemoryBytes: 40 * GiB}, Disk{Avail: 1000 * GiB}); n != 7 {
		t.Errorf("CPU-bound Fits = %d, want 7", n)
	}
	if n := Fits(c, Host{CPUSlots: 64, MemoryBytes: 20 * GiB}, Disk{Avail: 1000 * GiB}); n != 5 {
		t.Errorf("memory-bound Fits = %d, want 5", n)
	}
	if n := Fits(c, Host{CPUSlots: 64, MemoryBytes: 400 * GiB}, Disk{Avail: 50 * GiB}); n != 4 {
		t.Errorf("disk-bound Fits = %d, want 4", n)
	}
}

func TestCIIsAPlaceholder(t *testing.T) {
	if CI.Measured {
		t.Fatal("CI class marked measured before its peaks are measured")
	}
	if CI.VCPU != 2 || CI.MemoryBytes != GiB {
		t.Fatalf("CI placeholder = %+v, want 2 vCPU and 1 GiB", CI)
	}
}
