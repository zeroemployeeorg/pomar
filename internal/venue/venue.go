// Package venue owns Pomar's data root: the one directory under which every
// VM, image, volume and download Pomar creates must live, and the cleanup
// ledger that records each object before it exists.
//
// The rules it enforces:
//   - an object's intent is written, and synced, to the ledger before the
//     object is created;
//   - teardown removes only objects the ledger shows Pomar created, and only
//     paths inside the data root;
//   - heavy work is refused while the data root's filesystem is too full.
package venue

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultMaxFillPercent is the fill level above which heavy work is refused.
const DefaultMaxFillPercent = 85

// Kind names the class of object a ledger entry is about.
type Kind string

const (
	KindVM       Kind = "vm"
	KindImage    Kind = "image"
	KindVolume   Kind = "volume"
	KindDownload Kind = "download"
	KindNetwork  Kind = "network"
)

// Op is a ledger operation.
type Op string

const (
	OpIntent  Op = "intent"  // about to create; written before creation
	OpCreated Op = "created" // creation succeeded
	OpFailed  Op = "failed"  // creation failed; any partial path is still ours
	OpRemoved Op = "removed" // torn down with its data
)

// Entry is one ledger line.
type Entry struct {
	Seq  int       `json:"seq"`
	Time time.Time `json:"time"`
	Op   Op        `json:"op"`
	Kind Kind      `json:"kind"`
	ID   string    `json:"id"`
	// Path is relative to the data root; empty for objects with no data on
	// disk (for example a transient network).
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
}

// Object is the current state of one ledgered object.
type Object struct {
	Kind Kind
	ID   string
	Path string
	Last Op
}

// Venue is an open data root.
type Venue struct {
	root string
	mu   sync.Mutex
	seq  int
	objs map[string]*Object // key: kind/id
	now  func() time.Time
}

const ledgerName = "ledger.jsonl"

// Open opens the data root, which must already exist, be a directory and be
// given as an absolute path. It replays the ledger.
func Open(root string) (*Venue, error) {
	if root == "" {
		return nil, errors.New("venue: data root not set")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("venue: data root %q is not absolute", root)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("venue: %w", err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("venue: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("venue: data root %q is not a directory", real)
	}
	v := &Venue{root: real, objs: map[string]*Object{}, now: time.Now}
	if err := v.replay(); err != nil {
		return nil, err
	}
	return v, nil
}

// Root returns the resolved data root.
func (v *Venue) Root() string { return v.root }

func key(k Kind, id string) string { return string(k) + "/" + id }

func (v *Venue) replay() error {
	f, err := os.Open(filepath.Join(v.root, ledgerName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("venue: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return fmt.Errorf("venue: ledger line %d: %w", line, err)
		}
		v.apply(e)
	}
	return sc.Err()
}

func (v *Venue) apply(e Entry) {
	if e.Seq > v.seq {
		v.seq = e.Seq
	}
	k := key(e.Kind, e.ID)
	o, ok := v.objs[k]
	if !ok {
		o = &Object{Kind: e.Kind, ID: e.ID, Path: e.Path}
		v.objs[k] = o
	}
	if e.Op == OpIntent {
		o.Path = e.Path // an id reused after teardown starts afresh
	}
	o.Last = e.Op
}

// append writes one entry and syncs the ledger before returning.
func (v *Venue) append(e Entry) error {
	v.seq++
	e.Seq = v.seq
	e.Time = v.now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(v.root, ledgerName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("venue: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("venue: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("venue: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("venue: %w", err)
	}
	v.apply(e)
	return nil
}

// resolve checks that rel names a path strictly inside the data root and
// returns its absolute form.
func (v *Venue) resolve(rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("venue: path %q must be relative to the data root", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ledgerName || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("venue: path %q is not inside the data root", rel)
	}
	return filepath.Join(v.root, clean), nil
}

// Intent records that an object is about to be created. It must be called,
// and must succeed, before the object is created.
func (v *Venue) Intent(k Kind, id, rel, note string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if id == "" {
		return errors.New("venue: empty id")
	}
	if _, err := v.resolve(rel); err != nil {
		return err
	}
	if o, ok := v.objs[key(k, id)]; ok && o.Last != OpRemoved {
		return fmt.Errorf("venue: %s is already open (%s)", key(k, id), o.Last)
	}
	return v.append(Entry{Op: OpIntent, Kind: k, ID: id, Path: rel, Note: note})
}

// Created records that creation succeeded.
func (v *Venue) Created(k Kind, id string) error { return v.mark(k, id, OpCreated, "") }

// Failed records that creation failed. The object stays open until Teardown.
func (v *Venue) Failed(k Kind, id, note string) error { return v.mark(k, id, OpFailed, note) }

func (v *Venue) mark(k Kind, id string, op Op, note string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.objs[key(k, id)]
	if !ok || o.Last == OpRemoved {
		return fmt.Errorf("venue: %s has no open intent", key(k, id))
	}
	return v.append(Entry{Op: op, Kind: k, ID: id, Path: o.Path, Note: note})
}

// Teardown removes an open object's data and records the removal. Only an
// object the ledger shows as ours is touched, and only inside the data root.
// A symlink at the object's path is removed as a link, never followed.
func (v *Venue) Teardown(k Kind, id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.objs[key(k, id)]
	if !ok || o.Last == OpRemoved {
		return fmt.Errorf("venue: %s is not an open ledgered object", key(k, id))
	}
	abs, err := v.resolve(o.Path)
	if err != nil {
		return err
	}
	if abs != "" {
		if err := v.checkNoSymlinkParents(abs); err != nil {
			return err
		}
		if err := os.RemoveAll(abs); err != nil {
			return fmt.Errorf("venue: removing %s: %w", o.Path, err)
		}
	}
	return v.append(Entry{Op: OpRemoved, Kind: k, ID: id, Path: o.Path})
}

// checkNoSymlinkParents refuses paths whose parent directories, below the
// root, include a symlink that could redirect removal outside the root.
func (v *Venue) checkNoSymlinkParents(abs string) error {
	dir := filepath.Dir(abs)
	for dir != v.root {
		fi, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			dir = filepath.Dir(dir)
			continue
		}
		if err != nil {
			return fmt.Errorf("venue: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("venue: %s has a symlinked parent; refusing", abs)
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

// OpenObjects returns the objects not yet torn down, in no particular order.
func (v *Venue) OpenObjects() []Object {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []Object
	for _, o := range v.objs {
		if o.Last != OpRemoved {
			out = append(out, *o)
		}
	}
	return out
}

// Fill reports how full the data root's filesystem is, in percent: blocks in
// use over blocks in use plus blocks available to unprivileged users, rounded
// up. On APFS the in-use count covers every volume in the container, whose
// free space is shared, so this can read a few points above df's Capacity for
// one volume. That is deliberate: it is the figure that predicts exhaustion.
func (v *Venue) Fill() (int, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(v.root, &s); err != nil {
		return 0, fmt.Errorf("venue: statfs: %w", err)
	}
	used := s.Blocks - s.Bfree
	denom := used + s.Bavail
	if denom == 0 {
		return 0, errors.New("venue: statfs reported no blocks")
	}
	return int((used*100 + denom - 1) / denom), nil
}

// ErrDiskFull is returned by CheckHeavy when the fill limit is exceeded.
var ErrDiskFull = errors.New("venue: data root filesystem above fill limit")

// CheckHeavy refuses heavy work when the filesystem is above maxPercent full.
func (v *Venue) CheckHeavy(maxPercent int) error {
	pct, err := v.Fill()
	if err != nil {
		return err
	}
	if pct > maxPercent {
		return fmt.Errorf("%w: %d%% > %d%%", ErrDiskFull, pct, maxPercent)
	}
	return nil
}
