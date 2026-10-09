// Package seatapi serves the owner's seat routes (POMAR-CC SOW 15 §9 as
// corrected by SOW 16 §1, §4 and §9), on the development host's owner-only
// socket. A seat's declaration and its location are each add-only records,
// changed only by compare-and-set: a declaration on its previous sha256, a
// location on its revision. These routes record and report; they start,
// stop and grant nothing. Starting a seat is a later route.
package seatapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/calljournal"
	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/seatdecl"
)

// Store holds the seats under one private root:
// ROOT/declarations/<seat>/declaration-NNNNNNNNNNNN.json, add-only, and
// ROOT/locations (seatdecl.Locations).
type Store struct{ Root string }

var seatName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ErrChanged is a compare-and-set refusal.
var ErrChanged = errors.New("the seat changed since it was read; read it again")

// Declared is one seat's current declaration, as recorded.
type Declared struct {
	Seat     string               `json:"seat"`
	Revision int                  `json:"declaration_revision"`
	SHA256   string               `json:"declaration_sha256"`
	Body     seatdecl.Declaration `json:"declaration"`
}

func (s Store) dir(seat string) string { return filepath.Join(s.Root, "declarations", seat) }

// checkDir checks one of the store's directories: owner-private and not a
// link. A missing directory is created only when create is set; otherwise
// it is reported absent, and nothing is written.
func checkDir(p string, create bool) (present bool, err error) {
	if create {
		if err := os.Mkdir(p, 0o700); err != nil && !os.IsExist(err) {
			return false, err
		}
	} else if _, err := os.Lstat(p); os.IsNotExist(err) {
		return false, nil
	}
	if err := localclient.PrivateDir(p); err != nil {
		return false, err
	}
	return true, nil
}

// readable checks the store for a read: it never creates anything.
func (s Store) readable() (declarations, locations bool, err error) {
	if err := localclient.PrivateDir(s.Root); err != nil {
		return false, false, err
	}
	if declarations, err = checkDir(filepath.Join(s.Root, "declarations"), false); err != nil {
		return false, false, err
	}
	locations, err = checkDir(filepath.Join(s.Root, "locations"), false)
	return declarations, locations, err
}

// writable prepares the store for a write.
func (s Store) writable() error {
	if err := localclient.PrivateDir(s.Root); err != nil {
		return err
	}
	for _, d := range []string{"declarations", "locations"} {
		if _, err := checkDir(filepath.Join(s.Root, d), true); err != nil {
			return err
		}
	}
	return nil
}

// Current returns the seat's latest declaration, or ok false if none.
func (s Store) Current(seat string) (Declared, bool, error) {
	if !seatName.MatchString(seat) {
		return Declared{}, false, errors.New("invalid seat name")
	}
	declarations, _, err := s.readable()
	if err != nil || !declarations {
		return Declared{}, false, err
	}
	// The seat's own directory is checked before it is read, even when
	// empty: a link or an open mode is refused, never followed.
	present, err := checkDir(s.dir(seat), false)
	if err != nil || !present {
		return Declared{}, false, err
	}
	entries, err := os.ReadDir(s.dir(seat))
	if err != nil {
		return Declared{}, false, err
	}
	n := 0
	for i, e := range entries {
		if e.Name() != fmt.Sprintf("declaration-%012d.json", i+1) {
			return Declared{}, false, errors.New("the declaration record has a gap or a stray file")
		}
		n = i + 1
	}
	if n == 0 {
		return Declared{}, false, nil
	}
	raw, err := localclient.ReadPrivate(filepath.Join(s.dir(seat), fmt.Sprintf("declaration-%012d.json", n)), seatdecl.Limit)
	if err != nil {
		return Declared{}, false, err
	}
	d, sum, err := seatdecl.Parse(raw)
	if err != nil || d.Seat != seat {
		return Declared{}, false, errors.New("recorded declaration invalid; inspect")
	}
	return Declared{Seat: seat, Revision: n, SHA256: sum, Body: d}, true, nil
}

// Declare records raw as the seat's next declaration, only if the current
// one still has the expected sha256 ("" for a seat with none). The bytes
// are kept exactly, so the recorded sha256 stays their identity.
func (s Store) Declare(seat, expected string, raw []byte) (Declared, error) {
	d, sum, err := seatdecl.Parse(raw)
	if err != nil {
		return Declared{}, err
	}
	if d.Seat != seat {
		return Declared{}, errors.New("the declaration names another seat")
	}
	cur, ok, err := s.Current(seat)
	if err != nil {
		return Declared{}, err
	}
	if (ok && cur.SHA256 != expected) || (!ok && expected != "") {
		return Declared{}, ErrChanged
	}
	if ok && cur.SHA256 == sum {
		return cur, nil // the same bytes again: no new revision
	}
	if err := s.writable(); err != nil {
		return Declared{}, err
	}
	// Created or found, the seat's directory must be private and not a
	// link before anything is written into it.
	if _, err := checkDir(s.dir(seat), true); err != nil {
		return Declared{}, err
	}
	next := cur.Revision + 1
	f, err := os.OpenFile(filepath.Join(s.dir(seat), fmt.Sprintf("declaration-%012d.json", next)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return Declared{}, ErrChanged
	} else if err != nil {
		return Declared{}, err
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Declared{}, fmt.Errorf("declaration revision %d may be partial; inspect: %w", next, err)
	}
	if err := calljournal.Sync(s.dir(seat)); err != nil {
		return Declared{}, err
	}
	return Declared{Seat: seat, Revision: next, SHA256: sum, Body: d}, nil
}

func (s Store) locations() seatdecl.Locations {
	return seatdecl.Locations{Dir: filepath.Join(s.Root, "locations")}
}

// Seat is one seat's report: its declaration and location, as recorded.
// Environment and session state are reported by the route that starts
// seats, not here.
type Seat struct {
	Seat                string             `json:"seat"`
	DeclarationRevision int                `json:"declaration_revision"`
	DeclarationSHA256   string             `json:"declaration_sha256"`
	Role                string             `json:"role"`
	ProviderAccount     string             `json:"provider_account"`
	Location            *seatdecl.Location `json:"location,omitempty"`
	LocationCurrent     bool               `json:"location_matches_declaration"`
}

func (s Store) report(seat string) (Seat, bool, error) {
	d, ok, err := s.Current(seat)
	if err != nil || !ok {
		return Seat{}, ok, err
	}
	r := Seat{Seat: seat, DeclarationRevision: d.Revision, DeclarationSHA256: d.SHA256, Role: d.Body.Identity.Role, ProviderAccount: d.Body.Provider.Account}
	_, locations, err := s.readable()
	if err != nil {
		return Seat{}, false, err
	}
	if !locations {
		return r, true, nil
	}
	loc, has, err := s.locations().Current(seat)
	if err != nil {
		return Seat{}, false, err
	}
	if has {
		r.Location = &loc
		r.LocationCurrent = loc.DeclarationSHA256 == d.SHA256
	}
	return r, true, nil
}

// List reports every declared seat.
func (s Store) List() ([]Seat, error) {
	declarations, _, err := s.readable()
	if err != nil {
		return nil, err
	}
	out := []Seat{}
	if !declarations {
		return out, nil
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "declarations"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !seatName.MatchString(e.Name()) {
			return nil, fmt.Errorf("stray entry %q in the declarations; inspect", e.Name())
		}
		r, ok, err := s.report(e.Name())
		if err != nil {
			return nil, fmt.Errorf("seat %s: %w", e.Name(), err)
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// Handler serves the seat routes.
func (s Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/seats", func(w http.ResponseWriter, r *http.Request) {
		seats, err := s.List()
		if err != nil {
			respond(w, 500, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 200, map[string]any{"seats": seats})
	})
	mux.HandleFunc("GET /v1/seats/{seat}", func(w http.ResponseWriter, r *http.Request) {
		d, ok, err := s.Current(r.PathValue("seat"))
		if err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			respond(w, 404, map[string]string{"error": "unknown seat"})
			return
		}
		rep, _, err := s.report(d.Seat)
		if err != nil {
			respond(w, 500, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 200, map[string]any{"seat": rep, "declaration": d.Body})
	})
	mux.HandleFunc("PUT /v1/seats/{seat}/declaration", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Expected    string `json:"expected_sha256"`
			Declaration string `json:"declaration_base64"`
		}
		if !decode(w, r, &req) {
			return
		}
		raw, err := base64.StdEncoding.DecodeString(req.Declaration)
		if err != nil {
			respond(w, 400, map[string]string{"error": "declaration_base64 is the declaration's exact bytes, base64"})
			return
		}
		d, err := s.Declare(r.PathValue("seat"), req.Expected, raw)
		if errors.Is(err, ErrChanged) {
			respond(w, 409, map[string]string{"error": err.Error()})
			return
		} else if err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 200, map[string]any{"seat": d.Seat, "declaration_revision": d.Revision, "declaration_sha256": d.SHA256})
	})
	mux.HandleFunc("POST /v1/seats/{seat}/location", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Expected uint64            `json:"expected_revision"`
			Next     seatdecl.Location `json:"location"`
		}
		if !decode(w, r, &req) {
			return
		}
		seat := r.PathValue("seat")
		d, ok, err := s.Current(seat)
		if err != nil || !ok {
			respond(w, 400, map[string]string{"error": "a location is recorded only for a declared seat"})
			return
		}
		if req.Next.DeclarationSHA256 != d.SHA256 {
			respond(w, 409, map[string]string{"error": "the location must name the seat's current declaration"})
			return
		}
		if err := s.writable(); err != nil {
			respond(w, 500, map[string]string{"error": err.Error()})
			return
		}
		loc, err := s.locations().Move(seat, req.Expected, req.Next)
		if errors.Is(err, seatdecl.ErrRevision) {
			respond(w, 409, map[string]string{"error": err.Error()})
			return
		} else if err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 200, map[string]any{"location": loc})
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*seatdecl.Limit))
	if err != nil || len(body) >= 2*seatdecl.Limit {
		respond(w, 400, map[string]string{"error": "request too large or unreadable"})
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		respond(w, 400, map[string]string{"error": "invalid request: " + strings.SplitN(err.Error(), "\n", 2)[0]})
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		respond(w, 400, map[string]string{"error": "invalid request: trailing data"})
		return false
	}
	return true
}

func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
