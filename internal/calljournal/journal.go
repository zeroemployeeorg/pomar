// Package calljournal keeps immutable, privately retained client observations.
// The caller holds its operation lock throughout reading, dispatch and append.
package calljournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
)

const Limit = 12 << 20

type Event struct {
	ID    string          `json:"event_id"`
	Kind  string          `json:"kind"`
	At    string          `json:"observed_at"`
	Value json.RawMessage `json:"value"`
}

// Save publishes one complete record exclusively. Failed staging is retained,
// never silently discarded: it may contain a one-time response.
func Save(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > Limit {
		return errors.New("private record exceeds bound")
	}
	return SaveBytes(path, b, Limit)
}

// SaveBytes seals the exact bounded CI request without another encoding layer.
func SaveBytes(path string, b []byte, limit int) error {
	if len(b) > limit {
		return errors.New("private record exceeds bound")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = flush(f)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(f.Name(), path); err != nil {
		return err
	}
	if err = os.Remove(f.Name()); err != nil {
		return err
	}
	return Sync(filepath.Dir(path))
}
func Sync(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return flush(f)
}

func names(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pending-") {
			return nil, errors.New("incomplete private record retained; inspect before proceeding")
		}
		if strings.HasPrefix(e.Name(), "event-") {
			if e.IsDir() || len(e.Name()) != 22 || !strings.HasSuffix(e.Name(), ".json") {
				return nil, errors.New("invalid journal entry")
			}
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	if len(out) >= 4096 {
		return nil, errors.New("operation journal limit reached; preserve and inspect")
	}
	for i, n := range out {
		if n != fmt.Sprintf("event-%011d.json", i+1) {
			return nil, errors.New("operation journal gap")
		}
	}
	return out, nil
}
func Latest(dir string) (Event, bool, error) {
	return LatestKind(dir, "")
}

// LatestKind identifies retained evidence independently of the latest
// observation. An empty kind selects the latest event of any kind.
func LatestKind(dir, kind string) (Event, bool, error) {
	ns, err := names(dir)
	if err != nil {
		return Event{}, false, err
	}
	for i := len(ns) - 1; i >= 0; i-- {
		b, err := localclient.ReadPrivate(filepath.Join(dir, ns[i]), Limit)
		if err != nil {
			return Event{}, false, err
		}
		var e Event
		if json.Unmarshal(b, &e) != nil || e.ID != strings.TrimSuffix(ns[i], ".json") {
			return Event{}, false, errors.New("invalid journal identity")
		}
		if kind != "" && e.Kind != kind {
			continue
		}
		if err = Sync(dir); err != nil {
			return Event{}, false, err
		}
		return e, true, nil
	}
	return Event{}, false, nil
}
func Append(dir, kind string, value any) (Event, error) {
	ns, err := names(dir)
	if err != nil {
		return Event{}, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return Event{}, err
	}
	e := Event{ID: fmt.Sprintf("event-%011d", len(ns)+1), Kind: kind, At: time.Now().UTC().Format(time.RFC3339Nano), Value: b}
	return e, Save(filepath.Join(dir, e.ID+".json"), e)
}

// Complete finishes only an exact, fully written staged record. It never
// removes a partial/different file, nor overwrites an existing destination.
func Complete(path string, expected []byte) error {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		pending := filepath.Join(filepath.Dir(path), entry.Name())
		b, fi, e := readPending(pending, len(expected))
		if e != nil || !bytes.Equal(b, expected) {
			continue
		}
		if e = completeLink(pending, path, fi); e != nil {
			return e
		}
	}
	return nil
}
func readPending(path string, limit int) ([]byte, os.FileInfo, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	fi, e := f.Stat()
	if e != nil {
		return nil, nil, e
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || int(st.Uid) != os.Geteuid() || fi.Mode().Perm() != 0600 || st.Nlink < 1 || st.Nlink > 2 || fi.Size() > int64(limit) {
		return nil, nil, errors.New("staged record custody refused")
	}
	b := make([]byte, fi.Size())
	if _, e = f.ReadAt(b, 0); e != nil {
		return nil, nil, e
	}
	if e = flush(f); e != nil {
		return nil, nil, e
	}
	return b, fi, nil
}
func completeLink(pending, dst string, fi os.FileInfo) error {
	if old, e := os.Lstat(dst); e == nil {
		if !os.SameFile(old, fi) {
			return errors.New("conflicting staged destination")
		}
	} else if os.IsNotExist(e) {
		if e = os.Link(pending, dst); e != nil {
			return e
		}
	} else {
		return e
	}
	if e := os.Remove(pending); e != nil {
		return e
	}
	return Sync(filepath.Dir(dst))
}

// Recover preserves the event identity while completing a crashed publication.
// The owning client validates each event against its sealed original intent.
func Recover(dir string, validate func(Event) bool) error {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, fi, e := readPending(path, Limit)
		if e != nil {
			return errors.New("partial record retained; inspect before proceeding")
		}
		var event Event
		if json.Unmarshal(b, &event) != nil || len(event.ID) != 17 || !strings.HasPrefix(event.ID, "event-") || !validate(event) {
			return errors.New("staged event binding unresolved; retained for inspection")
		}
		if _, e = time.Parse(time.RFC3339Nano, event.At); e != nil {
			return errors.New("staged event time invalid")
		}
		name := event.ID + ".json"
		var sequence int
		if _, e = fmt.Sscanf(event.ID, "event-%011d", &sequence); e != nil || sequence < 1 || sequence > 4096 || fmt.Sprintf("event-%011d", sequence) != event.ID {
			return errors.New("staged event identity invalid")
		}
		if e = completeLink(path, filepath.Join(dir, name), fi); e != nil {
			return e
		}
	}
	return nil
}
