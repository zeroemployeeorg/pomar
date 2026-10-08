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
	a := append([]string{"--no-replace-objects", "--git-dir=" + mirror}, args...)
	c := exec.CommandContext(ctx, "git", a...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			c.Env = append(c.Env, v)
		}
	}
	c.Env = append(c.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
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
	t := tar.NewWriter(g)
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
	expanded, e := io.ReadAll(io.LimitReader(g, MaxExpanded+MaxManifest+(32<<20)+1))
	if e != nil || len(expanded) > MaxExpanded+MaxManifest+(32<<20) || buf.Len() != 0 {
		return m, errors.New("invalid or oversized gzip stream")
	}
	tarBytes := bytes.NewReader(expanded)
	t := tar.NewReader(tarBytes)
	seen := map[string]Entry{}
	manifestSeen := false
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
			continue
		}
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
		mode := "100644"
		if h.Mode == 0755 {
			mode = "100755"
		}
		seen[h.Name] = Entry{h.Name, mode, int64(len(b)), objectID("blob", b), sha256ID(b)}
	}
	for tarBytes.Len() > 0 {
		b, _ := tarBytes.ReadByte()
		if b != 0 {
			return m, errors.New("trailing non-tar data")
		}
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
