package seatdecl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/calljournal"
	"github.com/zeroemployeeorg/pomar/internal/localclient"
)

// Location is one revision of where a seat is active, kept apart from its
// immutable declaration (SOW 16 §1.2). Revisions are add-only: each is its
// own file, and none is ever rewritten. A move names the evidence it
// consumed (SOW 16 §4): the organisation's external hand-over receipt for
// the predecessor, and Pomar's own mechanism binding for the successor.
// This package records those references; it proves neither.
type Location struct {
	Seat              string `json:"seat"`
	Revision          uint64 `json:"revision"`
	Where             string `json:"where"`
	DeclarationSHA256 string `json:"declaration_sha256"`
	Handover          string `json:"handover,omitempty"`
	MechanismBinding  string `json:"mechanism_binding,omitempty"`
	At                string `json:"at"`
}

// ErrRevision is a compare-and-set refusal: the location moved since the
// caller read it, or another mover won.
var ErrRevision = errors.New("location revision changed; read it again")

var where = regexp.MustCompile(`^[a-z][a-z0-9:._-]{0,127}$`)

// Locations is a seat's add-only location record in a private directory.
type Locations struct{ Dir string }

func (l Locations) path(seat string, rev uint64) string {
	return filepath.Join(l.Dir, seat, fmt.Sprintf("location-%012d.json", rev))
}

// Current returns the seat's latest revision, or revision 0 with ok false
// if it has none.
func (l Locations) Current(seat string) (Location, bool, error) {
	if !seatName.MatchString(seat) {
		return Location{}, false, errors.New("invalid seat name")
	}
	if err := localclient.PrivateDir(l.Dir); err != nil {
		return Location{}, false, err
	}
	dir := filepath.Join(l.Dir, seat)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return Location{}, false, nil
	}
	// The seat's directory is checked before it is read: a link or an open
	// mode is refused, never followed.
	if err := localclient.PrivateDir(dir); err != nil {
		return Location{}, false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Location{}, false, err
	}
	var latest string
	for i, e := range entries {
		if strings.HasPrefix(e.Name(), ".pending-") {
			return Location{}, false, errors.New("an incomplete location record is retained; inspect before proceeding")
		}
		if e.Name() != fmt.Sprintf("location-%012d.json", i+1) {
			return Location{}, false, errors.New("the location record has a gap or a stray file")
		}
		latest = e.Name()
	}
	if latest == "" {
		return Location{}, false, nil
	}
	raw, err := localclient.ReadPrivate(filepath.Join(dir, latest), 64<<10)
	if err != nil {
		return Location{}, false, err
	}
	var loc Location
	if json.Unmarshal(raw, &loc) != nil || loc.Seat != seat || l.path(seat, loc.Revision) != filepath.Join(dir, latest) {
		return Location{}, false, errors.New("location record identity invalid")
	}
	return loc, true, nil
}

// Move records the next revision, only if the current one is still
// expected (0 for a seat with no location yet). A change of place needs
// both evidence references; the first revision needs neither, as no
// predecessor ran under Pomar.
func (l Locations) Move(seat string, expected uint64, next Location) (Location, error) {
	cur, ok, err := l.Current(seat)
	if err != nil {
		return Location{}, err
	}
	if (ok && cur.Revision != expected) || (!ok && expected != 0) {
		return Location{}, ErrRevision
	}
	if !where.MatchString(next.Where) || !digest.MatchString(next.DeclarationSHA256) {
		return Location{}, errors.New("a location names where, and the declaration's sha256")
	}
	if ok && cur.Where != next.Where && (strings.TrimSpace(next.Handover) == "" || strings.TrimSpace(next.MechanismBinding) == "") {
		return Location{}, errors.New("a move needs the external hand-over receipt and Pomar's mechanism binding (SOW 16 §4)")
	}
	next.Seat, next.Revision = seat, expected+1
	next.At = time.Now().UTC().Format(time.RFC3339Nano)
	dir := filepath.Join(l.Dir, seat)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return Location{}, err
	}
	if err := localclient.PrivateDir(dir); err != nil {
		return Location{}, err
	}
	// The exclusive create is the compare-and-set: of two movers from the
	// same revision, the second finds the file and writes nothing.
	b, err := json.Marshal(next)
	if err != nil {
		return Location{}, err
	}
	f, err := os.OpenFile(l.path(seat, next.Revision), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return Location{}, ErrRevision
	} else if err != nil {
		return Location{}, err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Location{}, fmt.Errorf("location revision %d may be partial; inspect before proceeding: %w", next.Revision, err)
	}
	return next, calljournal.Sync(dir)
}
