package manager

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
)

// Caller-specific class authorisation on the control socket (the council's
// ruling of 2026-10-01 23:11Z §3). The caller is the uid the kernel recorded
// for the connecting process (peerUID), never anything the client says. A
// class with an allow list may be started, read and stopped through the
// control socket only by the uids on it; the owner's socket is the operator's
// and is not scoped. A uid names a local OS account, not a seat that shares it.

// ReasonClassNotAllowed: the caller's uid is not on the class's allow list.
const ReasonClassNotAllowed = "class-not-allowed"

type callerKey struct{}

// caller is who a request comes from: the peer's uid when the kernel gave it,
// and whether the request came in on the owner's socket.
type caller struct {
	uid   uint32
	known bool
	owner bool
}

// connContext attaches the connecting peer to every request on the
// connection. An unreadable peer is kept as unknown, and refused below.
func connContext(owner bool) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		uid, err := peerUID(c)
		return context.WithValue(ctx, callerKey{}, caller{uid: uid, known: err == nil, owner: owner})
	}
}

func callerOf(r *http.Request) caller {
	c, _ := r.Context().Value(callerKey{}).(caller)
	return c
}

// checkClassAllow refuses, at the manager's start, an allow list for a class
// that does not exist, an empty one, and (with a control socket and more than
// one class) a class without one: a second class is never reachable through
// the shared socket without its own list.
func checkClassAllow(cfg Config) error {
	names := map[string]bool{}
	for _, jc := range cfg.Classes {
		names[jc.Class.Name] = true
	}
	for name, uids := range cfg.ClassAllow {
		if !names[name] {
			return fmt.Errorf("manager: an allow list names class %q, which this manager does not have", name)
		}
		if len(uids) == 0 {
			return fmt.Errorf("manager: class %q has an empty allow list", name)
		}
	}
	if cfg.CtlSocket != "" && len(cfg.Classes) > 1 {
		for _, jc := range cfg.Classes {
			if _, ok := cfg.ClassAllow[jc.Class.Name]; !ok {
				return fmt.Errorf("manager: with %d classes on the control socket, class %q needs an allow list", len(cfg.Classes), jc.Class.Name)
			}
		}
	}
	return nil
}

// mayUse reports whether c may start, read or stop attempts of class name.
func (m *Manager) mayUse(c caller, name string) bool {
	if c.owner {
		return true
	}
	if !c.known {
		return false // fail closed
	}
	uids, ok := m.cfg.ClassAllow[name]
	if !ok {
		return true // no list: only possible with one class, as today
	}
	return slices.Contains(uids, c.uid)
}

// mayUseAll reports whether c may use every class: the host-wide reads
// (caches, reconcile, VM orphans) name attempts of all of them.
func (m *Manager) mayUseAll(c caller) bool {
	for _, jc := range m.cfg.Classes {
		if !m.mayUse(c, jc.Class.Name) {
			return false
		}
	}
	return true
}

// refuseClass answers 403 class-not-allowed, naming the class and the uid.
func refuseClass(w http.ResponseWriter, c caller, class string) {
	who := "an unidentified caller"
	if c.known {
		who = fmt.Sprintf("uid %d", c.uid)
	}
	reply(w, http.StatusForbidden, map[string]string{
		"error":  fmt.Sprintf("manager: %s may not use class %q", who, class),
		"reason": ReasonClassNotAllowed,
	})
}

// attemptGuard wraps a route on one attempt: an attempt of a class the
// caller may not use is refused before the route runs. A missing attempt is
// left to the route, which answers 404.
func (m *Manager) attemptGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := callerOf(r)
		if e, ok := m.Get(r.PathValue("id")); ok {
			if class := e.claimClass().Name; !m.mayUse(c, class) {
				refuseClass(w, c, class)
				return
			}
		}
		next(w, r)
	}
}

// hostWide wraps a host-wide read: only a caller who may use every class.
func (m *Manager) hostWide(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c := callerOf(r); !m.mayUseAll(c) {
			refuseClass(w, c, "every class")
			return
		}
		next(w, r)
	}
}
