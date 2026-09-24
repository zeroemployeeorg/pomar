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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// Class is an object's lifecycle. Attempt objects are torn down when their
// attempt ends; cache objects persist and are evicted only by the retention
// policy, never by attempt teardown.
type Class string

const (
	ClassAttempt Class = "attempt"
	ClassCache   Class = "cache"
)

func (c Class) valid() bool { return c == ClassAttempt || c == ClassCache }

// Structure is the fixed set of directories that give the data root its
// shape. They are not ledgered: they hold only ledgered objects, Init creates
// them, and a directory not listed here is not structure.
var Structure = []string{"attempts", "bases", "downloads", "kernels", "mirrors", "volumes"}

// Op is a ledger operation.
type Op string

const (
	OpIntent   Op = "intent"   // about to create; written before creation
	OpCreated  Op = "created"  // creation succeeded
	OpFailed   Op = "failed"   // creation failed; any partial path is still ours
	OpRemoved  Op = "removed"  // torn down with its data
	OpClassify Op = "classify" // assigns a class to an object ledgered before classes existed
)

// Entry is one ledger line.
type Entry struct {
	Seq   int       `json:"seq"`
	Time  time.Time `json:"time"`
	Op    Op        `json:"op"`
	Kind  Kind      `json:"kind"`
	Class Class     `json:"class,omitempty"`
	ID    string    `json:"id"`
	// Path is relative to the data root; empty for objects with no data on
	// disk (for example a transient network).
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
}

// Object is the current state of one ledgered object.
type Object struct {
	Kind  Kind   `json:"kind"`
	Class Class  `json:"class"` // empty only for objects ledgered before classes existed
	ID    string `json:"id"`
	Path  string `json:"path"`
	Last  Op     `json:"last"`
	// Created is when creation last succeeded; eviction goes oldest first.
	Created time.Time `json:"created"`
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
	switch e.Op {
	case OpIntent:
		o.Path = e.Path // an id reused after teardown starts afresh
		o.Class = e.Class
	case OpClassify:
		o.Class = e.Class
		return // classifying does not change the lifecycle state
	case OpCreated:
		o.Created = e.Time
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
func (v *Venue) Intent(k Kind, c Class, id, rel, note string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if id == "" {
		return errors.New("venue: empty id")
	}
	if !c.valid() {
		return fmt.Errorf("venue: invalid class %q", c)
	}
	if _, err := v.resolve(rel); err != nil {
		return err
	}
	if o, ok := v.objs[key(k, id)]; ok && o.Last != OpRemoved {
		return fmt.Errorf("venue: %s is already open (%s)", key(k, id), o.Last)
	}
	return v.append(Entry{Op: OpIntent, Kind: k, Class: c, ID: id, Path: rel, Note: note})
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
	return v.append(Entry{Op: op, Kind: k, Class: o.Class, ID: id, Path: o.Path, Note: note})
}

// Teardown removes an open object's data and records the removal. Only an
// object the ledger shows as ours is touched, and only inside the data root.
// A symlink at the object's path is removed as a link, never followed.
func (v *Venue) Teardown(k Kind, id string) error { return v.TeardownNote(k, id, "") }

// TeardownNote is Teardown with a reason recorded in the ledger, such as an
// eviction's budget.
func (v *Venue) TeardownNote(k Kind, id, note string) error {
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
	return v.append(Entry{Op: OpRemoved, Kind: k, Class: o.Class, ID: id, Path: o.Path, Note: note})
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

// Space is the data root's filesystem as admission sees it.
type Space struct {
	Used  int64 `json:"used"`  // bytes in use, across the APFS container
	Avail int64 `json:"avail"` // bytes available to unprivileged users
	// Device is the data root's device (for example /dev/disk3s5), and
	// Shared reports whether it sits in the same APFS container as the
	// system's data volume, so that its free space is the operator's too.
	Device string `json:"device"`
	Shared bool   `json:"shared"`
}

// SystemData is the volume that holds the operator's data on macOS.
const SystemData = "/System/Volumes/Data"

// Space reads the data root's filesystem. Shared is true unless the data
// root is shown to be on another container than SystemData: a figure that
// cannot be read counts as shared, which keeps the fill floor on.
func (v *Venue) Space() (Space, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(v.root, &s); err != nil {
		return Space{}, fmt.Errorf("venue: statfs: %w", err)
	}
	bs := int64(s.Bsize)
	sp := Space{Used: int64(s.Blocks-s.Bfree) * bs, Avail: int64(s.Bavail) * bs, Device: cstr(s.Mntfromname[:]), Shared: true}
	var sys syscall.Statfs_t
	if err := syscall.Statfs(SystemData, &sys); err == nil {
		sp.Shared = SameContainer(sp.Device, cstr(sys.Mntfromname[:]))
	}
	return sp, nil
}

// SameContainer reports whether two /dev/diskNs… devices are volumes of one
// APFS container (disk3s1s1 and disk3s5 are; disk3s5 and disk6s1 are not).
// Anything it cannot parse counts as the same container.
func SameContainer(a, b string) bool {
	ca, oka := container(a)
	cb, okb := container(b)
	return !oka || !okb || ca == cb
}

func container(dev string) (string, bool) {
	d, ok := strings.CutPrefix(dev, "/dev/disk")
	if !ok {
		return "", false
	}
	n := 0
	for n < len(d) && d[n] >= '0' && d[n] <= '9' {
		n++
	}
	if n == 0 {
		return "", false
	}
	return d[:n], true
}

func cstr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
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

// Classify gives a class to an open object ledgered before classes existed.
// It refuses an object that already has one.
func (v *Venue) Classify(k Kind, id string, c Class) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !c.valid() {
		return fmt.Errorf("venue: invalid class %q", c)
	}
	o, ok := v.objs[key(k, id)]
	if !ok || o.Last == OpRemoved {
		return fmt.Errorf("venue: %s is not an open ledgered object", key(k, id))
	}
	if o.Class != "" {
		return fmt.Errorf("venue: %s already has class %s", key(k, id), o.Class)
	}
	return v.append(Entry{Op: OpClassify, Kind: k, Class: c, ID: id, Path: o.Path})
}

// Init creates the structure directories. It is idempotent, and it is the
// only code that creates anything in the data root without a ledger entry.
func (v *Venue) Init() error {
	for _, d := range Structure {
		if err := os.MkdirAll(filepath.Join(v.root, d), 0o700); err != nil {
			return fmt.Errorf("venue: %w", err)
		}
	}
	return nil
}

// Unaccounted walks the data root and returns, relative to it, every entry
// that is neither structure, nor an open ledgered object or inside one, nor a
// directory on the way to one. It reports; it never deletes. Symlinks are not
// followed.
func (v *Venue) Unaccounted() ([]string, error) {
	v.mu.Lock()
	var open []string
	for _, o := range v.objs {
		if o.Last != OpRemoved && o.Path != "" {
			open = append(open, filepath.Clean(o.Path))
		}
	}
	v.mu.Unlock()
	structure := map[string]bool{}
	for _, d := range Structure {
		structure[d] = true
	}
	var out []string
	err := filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(v.root, p)
		if rel == "." || rel == ledgerName {
			return nil
		}
		for _, o := range open {
			if rel == o || strings.HasPrefix(rel, o+string(filepath.Separator)) {
				if d.IsDir() {
					return filepath.SkipDir // inside an object: its contents are the object
				}
				return nil
			}
		}
		if d.IsDir() && d.Type()&fs.ModeSymlink == 0 {
			if structure[rel] {
				return nil
			}
			for _, o := range open {
				if strings.HasPrefix(o, rel+string(filepath.Separator)) {
					return nil // on the way to an object; its other contents are still checked
				}
			}
			out = append(out, rel)
			return filepath.SkipDir
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// IsOpen reports whether an object is ledgered and not yet torn down.
func (v *Venue) IsOpen(k Kind, id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	o, ok := v.objs[key(k, id)]
	return ok && o.Last != OpRemoved
}

// EnsureCache ledgers a long-lived cache directory once, before creating it.
func (v *Venue) EnsureCache(k Kind, id, rel string) error {
	if v.IsOpen(k, id) {
		return os.MkdirAll(filepath.Join(v.root, rel), 0o700)
	}
	if err := v.Intent(k, ClassCache, id, rel, ""); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(v.root, rel), 0o700); err != nil {
		return fmt.Errorf("venue: %w", err)
	}
	return v.Created(k, id)
}
