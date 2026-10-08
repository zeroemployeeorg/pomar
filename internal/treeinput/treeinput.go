// Package treeinput materialises an exact owner-mirror tree without Git history.
// A manifest binds every included byte and executable bit to the pinned commit.
package treeinput

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const MaxArchive = 32 << 20
const MaxExpanded = 256 << 20
const MaxManifest = 8 << 20
const ManifestName = ".pomar-tree-manifest.json"

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Entry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	Blob   string `json:"git_blob"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Version      int     `json:"version"`
	Commit       string  `json:"commit"`
	Tree         string  `json:"tree"`
	CommitObject []byte  `json:"commit_object"`
	Entries      []Entry `json:"entries"`
}

func validPath(p string) bool {
	if p == "" || len(p) > 4096 || !utf8.ValidString(p) || strings.Count(p, "/") > 127 || p == ManifestName || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n\t") {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if len(s) > 255 || s == ".." || s == "." || strings.EqualFold(s, ".git") {
			return false
		}
	}
	return true
}
func objectID(kind string, b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func sha256ID(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// git reads local objects only. No fetch, remote, credential helper, replacement
// objects, hooks, or optional Git locks are used. Diagnostics omit Git output.
func git(ctx context.Context, mirror string, limit int64, args ...string) ([]byte, error) {
	a := append([]string{"--no-replace-objects", "--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "protocol.allow=never", "--git-dir=" + mirror}, args...)
	c := exec.CommandContext(ctx, "git", a...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			c.Env = append(c.Env, v)
		}
	}
	c.Env = append(c.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_NO_LAZY_FETCH=1", "GIT_ATTR_NOSYSTEM=1")
	r, err := c.StdoutPipe()
	if err != nil {
		return nil, errors.New("local Git pipe unavailable")
	}
	if c.Start() != nil {
		return nil, errors.New("local Git unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(b)) > limit {
		c.Process.Kill()
		c.Wait()
		return nil, errors.New("local Git object exceeds bound")
	}
	if c.Wait() != nil {
		return nil, errors.New("local Git object/ref check refused")
	}
	return b, nil
}

type boundedWriter struct {
	w    io.Writer
	left int64
}

func (w *boundedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.left {
		return 0, errors.New("compressed input exceeds bound")
	}
	n, e := w.w.Write(b)
	w.left -= int64(n)
	return n, e
}

// Build requires a bare mirror, an exact full branch ref and a reachable commit.
// It walks actual Git objects rather than git archive: export-ignore and
// export-subst cannot omit or rewrite tracked files. Symlinks/gitlinks refuse.
func Build(ctx context.Context, mirror, ref, commit, dst string, required []string, maxBytes int64) (Manifest, error) {
	var m Manifest
	if !filepath.IsAbs(mirror) || !fullSHA.MatchString(commit) || !strings.HasPrefix(ref, "refs/heads/") || strings.ContainsAny(ref, "\x00\r\n") || maxBytes < 1 || maxBytes > MaxArchive {
		return m, errors.New("invalid mirror/ref/commit/size bound")
	}
	view, cleanup, e := isolatedMirror(ctx, mirror, ref, filepath.Dir(dst))
	if e != nil {
		return m, e
	}
	defer cleanup()
	mirror = view
	if b, e := git(ctx, mirror, 128, "rev-parse", "--is-bare-repository"); e != nil || string(b) != "true\n" {
		return m, errors.New("an existing bare owner mirror is required")
	}
	if _, e := git(ctx, mirror, 128, "check-ref-format", ref); e != nil {
		return m, e
	}
	if _, e := git(ctx, mirror, 128, "merge-base", "--is-ancestor", commit, ref); e != nil {
		return m, errors.New("pinned commit is not reachable from owner branch")
	}
	commitBytes, e := git(ctx, mirror, 128<<10, "cat-file", "commit", commit)
	if e != nil {
		return m, e
	}
	if objectID("commit", commitBytes) != commit {
		return m, errors.New("commit object identity mismatch")
	}
	line, _, ok := bytes.Cut(commitBytes, []byte("\n"))
	if !ok || !bytes.HasPrefix(line, []byte("tree ")) || !fullSHA.Match(line[5:]) {
		return m, errors.New("invalid commit tree")
	}
	m = Manifest{Version: 1, Commit: commit, Tree: string(line[5:]), CommitObject: commitBytes, Entries: []Entry{}}
	listing, e := git(ctx, mirror, MaxManifest, "ls-tree", "-rz", "--full-tree", commit)
	if e != nil {
		return m, e
	}
	var total int64
	for _, record := range bytes.Split(listing, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, p, ok := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 || !validPath(string(p)) || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !fullSHA.MatchString(fields[2]) {
			return m, errors.New("unsupported or unsafe tree entry")
		}
		m.Entries = append(m.Entries, Entry{Path: string(p), Mode: fields[0], Blob: fields[2]})
		if len(m.Entries) > 100000 {
			return m, errors.New("tree entry bound exceeded")
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	if e = portablePaths(m.Entries); e != nil {
		return m, e
	}
	if e = requiredEntries(m, required); e != nil {
		return m, e
	}
	// Exclusive creation preserves an existing input; partial own outputs are
	// removed on failure, never another path or an existing artifact.
	f, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return m, errors.New("new private output required")
	}
	done := false
	defer func() {
		f.Close()
		if !done {
			os.Remove(dst)
		}
	}()
	g := gzip.NewWriter(&boundedWriter{f, maxBytes})
	t := tar.NewWriter(&boundedWriter{g, MaxExpanded + MaxManifest + (32 << 20)})
	for i := range m.Entries {
		x := &m.Entries[i]
		b, e := git(ctx, mirror, MaxExpanded-total, "cat-file", "blob", x.Blob)
		if e != nil {
			return m, e
		}
		total += int64(len(b))
		if objectID("blob", b) != x.Blob {
			return m, errors.New("blob identity mismatch")
		}
		x.Size = int64(len(b))
		x.SHA256 = sha256ID(b)
		mode := int64(0644)
		if x.Mode == "100755" {
			mode = 0755
		}
		if e = t.WriteHeader(&tar.Header{Name: x.Path, Mode: mode, Size: x.Size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); e != nil {
			return m, e
		}
		if _, e = t.Write(b); e != nil {
			return m, e
		}
	}
	if treeID(m.Entries) != m.Tree {
		return m, errors.New("complete tree proof mismatch")
	}
	b, e := json.Marshal(m)
	if e != nil || len(b) > MaxManifest {
		return m, errors.New("manifest exceeds bound")
	}
	if e = t.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); e != nil {
		return m, e
	}
	if _, e = t.Write(b); e != nil {
		return m, e
	}
	if e = t.Close(); e != nil {
		return m, e
	}
	if e = g.Close(); e != nil {
		return m, e
	}
	if e = f.Sync(); e != nil {
		return m, e
	}
	if e = f.Close(); e != nil {
		return m, e
	}
	done = true
	return m, nil
}

// treeID reconstructs the Git Merkle tree, including modes and directory order.
func treeID(entries []Entry) string {
	type node struct {
		dirs  map[string]*node
		files map[string]Entry
	}
	newNode := func() *node { return &node{map[string]*node{}, map[string]Entry{}} }
	root := newNode()
	for _, e := range entries {
		if !validPath(e.Path) || !fullSHA.MatchString(e.Blob) || (e.Mode != "100644" && e.Mode != "100755") {
			return ""
		}
		parts := strings.Split(e.Path, "/")
		n := root
		for _, s := range parts[:len(parts)-1] {
			if _, ok := n.files[s]; ok {
				return ""
			}
			if n.dirs[s] == nil {
				n.dirs[s] = newNode()
			}
			n = n.dirs[s]
		}
		s := parts[len(parts)-1]
		if n.dirs[s] != nil {
			return ""
		}
		if _, ok := n.files[s]; ok {
			return ""
		}
		n.files[s] = e
	}
	var digest func(*node) string
	digest = func(n *node) string {
		names := []string{}
		for s := range n.dirs {
			names = append(names, s+"/")
		}
		for s := range n.files {
			names = append(names, s)
		}
		sort.Strings(names)
		var b bytes.Buffer
		for _, s := range names {
			mode := "40000"
			name := strings.TrimSuffix(s, "/")
			hash := ""
			if strings.HasSuffix(s, "/") {
				hash = digest(n.dirs[name])
			} else {
				e := n.files[s]
				mode, hash = e.Mode, e.Blob
			}
			raw, _ := hex.DecodeString(hash)
			fmt.Fprintf(&b, "%s %s\x00", mode, name)
			b.Write(raw)
		}
		return objectID("tree", b.Bytes())
	}
	return digest(root)
}
func requiredEntries(m Manifest, required []string) error {
	present := map[string]bool{}
	for _, e := range m.Entries {
		present[e.Path] = true
	}
	for _, p := range required {
		if !validPath(p) || !present[p] {
			return errors.New("required source or dependency lock is missing")
		}
	}
	return nil
}

// Verify checks the entire bounded archive against an independently supplied
// commit pin. It does not execute scripts, extract paths or certify dependencies.
func Verify(r io.Reader, expected string, required []string) (Manifest, error) {
	var m Manifest
	if !fullSHA.MatchString(expected) {
		return m, errors.New("exact expected commit required")
	}
	compressed, e := io.ReadAll(io.LimitReader(r, MaxArchive+1))
	if e != nil || len(compressed) > MaxArchive {
		return m, errors.New("archive exceeds compressed bound")
	}
	buf := bytes.NewReader(compressed)
	g, e := gzip.NewReader(buf)
	if e != nil {
		return m, errors.New("invalid gzip input")
	}
	g.Multistream(false)
	defer g.Close()
	limited := &io.LimitedReader{R: g, N: MaxExpanded + MaxManifest + (32 << 20) + 1}
	t := tar.NewReader(limited)
	canonicalHash := sha256.New()
	canonicalCount := &countWriter{w: canonicalHash}
	cg := gzip.NewWriter(canonicalCount)
	ct := tar.NewWriter(cg)
	seen := map[string]Entry{}
	manifestSeen := false
	previousPath := ""
	var total int64
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return m, errors.New("invalid tar input")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return m, errors.New("only regular files allowed")
		}
		if h.Name == ManifestName {
			if manifestSeen || h.Size > MaxManifest || h.Mode != 0600 {
				return m, errors.New("invalid manifest entry")
			}
			manifestSeen = true
			b, e := io.ReadAll(t)
			if e != nil {
				return m, e
			}
			d := json.NewDecoder(bytes.NewReader(b))
			d.DisallowUnknownFields()
			if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF {
				return m, errors.New("invalid manifest")
			}
			canonical, e := json.Marshal(m)
			if e != nil || !bytes.Equal(b, canonical) {
				return m, errors.New("noncanonical manifest")
			}
			if e = ct.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0600, Size: int64(len(canonical)), Typeflag: tar.TypeReg}); e != nil {
				return m, e
			}
			if _, e = ct.Write(canonical); e != nil {
				return m, e
			}
			continue
		}
		if manifestSeen {
			return m, errors.New("manifest must be last")
		}
		if h.Name <= previousPath {
			return m, errors.New("noncanonical entry order")
		}
		previousPath = h.Name
		if !validPath(h.Name) || h.Size < 0 || (h.Mode != 0644 && h.Mode != 0755) || len(seen) >= 100000 {
			return m, errors.New("unsafe tree entry")
		}
		if _, ok := seen[h.Name]; ok {
			return m, errors.New("duplicate tree entry")
		}
		total += h.Size
		if total > MaxExpanded {
			return m, errors.New("expanded source exceeds bound")
		}
		b, e := io.ReadAll(t)
		if e != nil {
			return m, e
		}
		if e = ct.WriteHeader(&tar.Header{Name: h.Name, Mode: h.Mode, Size: h.Size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); e != nil {
			return m, e
		}
		if _, e = ct.Write(b); e != nil {
			return m, e
		}
		mode := "100644"
		if h.Mode == 0755 {
			mode = "100755"
		}
		seen[h.Name] = Entry{h.Name, mode, int64(len(b)), objectID("blob", b), sha256ID(b)}
	}
	if _, e = io.Copy(io.Discard, limited); e != nil || limited.N <= 0 || buf.Len() != 0 {
		return m, errors.New("invalid or oversized gzip stream")
	}
	if e = ct.Close(); e != nil {
		return m, e
	}
	if e = cg.Close(); e != nil {
		return m, e
	}
	expectedHash := sha256.Sum256(compressed)
	if canonicalCount.n != int64(len(compressed)) || !bytes.Equal(canonicalHash.Sum(nil), expectedHash[:]) {
		return m, errors.New("archive is not canonical; metadata/order/padding refused")
	}
	if e = portablePaths(m.Entries); e != nil {
		return m, e
	}
	if !manifestSeen || m.Version != 1 || m.Commit != expected || objectID("commit", m.CommitObject) != expected || len(m.Entries) != len(seen) {
		return m, errors.New("manifest/commit identity mismatch")
	}
	line, _, ok := bytes.Cut(m.CommitObject, []byte("\n"))
	if !ok || string(line) != "tree "+m.Tree || treeID(m.Entries) != m.Tree {
		return m, errors.New("manifest tree proof mismatch")
	}
	for _, e := range m.Entries {
		if got, ok := seen[e.Path]; !ok || got != e {
			return m, errors.New("tree bytes/modes differ from manifest")
		}
	}
	if e = requiredEntries(m, required); e != nil {
		return m, e
	}
	return m, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(b []byte) (int, error) {
	n, e := w.w.Write(b)
	w.n += int64(n)
	return n, e
}

// The portable profile refuses case-folded file and directory collisions.
// Extraction additionally lets the actual destination filesystem reject any
// normalization aliases; it never overwrites a preexisting directory or file.
func portablePaths(entries []Entry) error {
	seen := map[string]string{}
	for _, e := range entries {
		parts := strings.Split(e.Path, "/")
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			key := strings.ToLower(p)
			if old, ok := seen[key]; ok && old != p {
				return errors.New("case-colliding source paths")
			}
			seen[key] = p
		}
	}
	return nil
}

// isolatedMirror exposes only local objects and one fixed ref to Git. The
// input repository's config, hooks, worktree and executable settings never
// become Git's repository configuration, even while its config is inspected.
func isolatedMirror(ctx context.Context, source, ref, parent string) (string, func(), error) {
	fail := func(e error) (string, func(), error) { return "", func() {}, e }
	if !validPath(ref) || !strings.HasPrefix(ref, "refs/heads/") {
		return fail(errors.New("unsafe branch ref"))
	}
	for _, p := range []string{source, filepath.Join(source, "objects"), filepath.Join(source, "config")} {
		fi, e := os.Lstat(p)
		if e != nil || fi.Mode()&os.ModeSymlink != 0 {
			return fail(errors.New("mirror custody unavailable"))
		}
	}
	for _, p := range []string{"shallow", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, e := os.Lstat(filepath.Join(source, p)); !os.IsNotExist(e) {
			return fail(errors.New("incomplete or alternate object store refused"))
		}
	}
	if e := filepath.WalkDir(filepath.Join(source, "objects"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 || strings.HasSuffix(d.Name(), ".promisor") {
			return errors.New("promisor or linked object store refused")
		}
		return nil
	}); e != nil {
		return fail(e)
	}
	view, e := os.MkdirTemp(parent, ".pomar-local-objects-")
	if e != nil {
		return fail(e)
	}
	cleanup := func() { os.RemoveAll(view) }
	refuse := func(e error) (string, func(), error) { cleanup(); return fail(e) }
	if e = os.WriteFile(filepath.Join(view, "config"), []byte("[core]\nrepositoryformatversion = 0\nbare = true\n"), 0600); e != nil {
		return refuse(e)
	}
	// Read configuration as inert data, without includes; no input config is
	// ever installed in the isolated repository.
	config, e := git(ctx, view, 1<<20, "config", "--no-includes", "--file", filepath.Join(source, "config"), "--null", "--list")
	if e != nil {
		return refuse(e)
	}
	bare := false
	for _, row := range bytes.Split(config, []byte{0}) {
		kv := bytes.SplitN(row, []byte{'\n'}, 2)
		if len(kv) == 0 {
			continue
		}
		key := strings.ToLower(string(kv[0]))
		value := ""
		if len(kv) == 2 {
			value = string(kv[1])
		}
		if key == "extensions.partialclone" || strings.HasSuffix(key, ".promisor") || (key == "extensions.objectformat" && value != "sha1") {
			return refuse(errors.New("promisor or unsupported mirror refused"))
		}
		if key == "core.bare" && value == "true" {
			bare = true
		}
	}
	if !bare {
		return refuse(errors.New("bare source mirror required"))
	}
	target := filepath.Join(source, filepath.FromSlash(ref))
	for p := filepath.Dir(target); p != source; p = filepath.Dir(p) {
		fi, e := os.Lstat(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return refuse(errors.New("ref custody refused"))
		}
	}
	var tip string
	if fi, e := os.Lstat(target); e == nil {
		if !fi.Mode().IsRegular() || fi.Size() > 128 {
			return refuse(errors.New("unsafe loose ref"))
		}
		b, e := os.ReadFile(target)
		if e != nil {
			return refuse(e)
		}
		tip = strings.TrimSpace(string(b))
	} else if !os.IsNotExist(e) {
		return refuse(e)
	}
	if tip == "" {
		fi, e := os.Lstat(filepath.Join(source, "packed-refs"))
		if e != nil || !fi.Mode().IsRegular() || fi.Size() > 8<<20 {
			return refuse(errors.New("packed-ref custody refused"))
		}
		packed, e := os.Open(filepath.Join(source, "packed-refs"))
		if e != nil {
			return refuse(e)
		}
		b, e := io.ReadAll(io.LimitReader(packed, (8<<20)+1))
		packed.Close()
		if e != nil || len(b) > 8<<20 {
			return refuse(errors.New("packed ref bound"))
		}
		for _, line := range strings.Split(string(b), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 2 && parts[1] == ref {
				if tip != "" {
					return refuse(errors.New("duplicate packed ref"))
				}
				tip = parts[0]
			}
		}
	}
	if !fullSHA.MatchString(tip) {
		return refuse(errors.New("full local branch tip required"))
	}
	if e = os.MkdirAll(filepath.Dir(filepath.Join(view, ref)), 0700); e != nil {
		return refuse(e)
	}
	if e = os.WriteFile(filepath.Join(view, ref), []byte(tip+"\n"), 0600); e != nil {
		return refuse(e)
	}
	if e = os.WriteFile(filepath.Join(view, "HEAD"), []byte("ref: "+ref+"\n"), 0600); e != nil {
		return refuse(e)
	}
	if e = os.Symlink(filepath.Join(source, "objects"), filepath.Join(view, "objects")); e != nil {
		return refuse(e)
	}
	return view, cleanup, nil
}

// Extract verifies a bounded immutable copy, then extracts that SAME copy to a
// new private root. os.Root confines every destination operation. Exclusive
// creation also enforces the destination filesystem's actual alias rules.
// An interrupted output remains marked incomplete and is never reused.
func Extract(r io.Reader, expected string, required []string, dst string) (Manifest, error) {
	var m Manifest
	compressed, e := io.ReadAll(io.LimitReader(r, MaxArchive+1))
	if e != nil || len(compressed) > MaxArchive {
		return m, errors.New("archive exceeds bound")
	}
	m, e = Verify(bytes.NewReader(compressed), expected, required)
	if e != nil {
		return m, e
	}
	if !filepath.IsAbs(dst) {
		return m, errors.New("absolute new extraction directory required")
	}
	if e = os.Mkdir(dst, 0700); e != nil {
		return m, errors.New("new extraction directory required")
	}
	root, e := os.OpenRoot(dst)
	if e != nil {
		return m, e
	}
	defer root.Close()
	marker := ".pomar-extraction-incomplete"
	mark, e := root.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return m, e
	}
	if e = mark.Sync(); e != nil {
		mark.Close()
		return m, e
	}
	mark.Close()
	g, e := gzip.NewReader(bytes.NewReader(compressed))
	if e != nil {
		return m, e
	}
	defer g.Close()
	t := tar.NewReader(g)
	dirs := map[string]bool{".": true}
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return m, e
		}
		parts := strings.Split(path.Dir(h.Name), "/")
		current := ""
		for _, part := range parts {
			current = path.Join(current, part)
			if dirs[current] {
				continue
			}
			if e = root.Mkdir(current, 0700); e != nil {
				return m, errors.New("destination directory collision or incomplete output")
			}
			dirs[current] = true
		}
		f, e := root.OpenFile(h.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode))
		if e != nil {
			return m, errors.New("destination file collision or incomplete output")
		}
		n, e := io.Copy(f, t)
		if e == nil && n != h.Size {
			e = errors.New("incomplete extraction")
		}
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return m, e
		}
		if ce != nil {
			return m, ce
		}
	}
	if e = root.Remove(marker); e != nil {
		return m, e
	}
	return m, nil
}
