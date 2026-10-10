//go:build darwin

package serviceupgrade

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/cicaller"
	"github.com/zeroemployeeorg/pomar/internal/manager"
)

const bindir = "/usr/local/libexec/pomar"
const serviceRoot = "/private/var/pomar"
const recordPath = serviceRoot + "/install-record"
const runDir = serviceRoot + "/run"
const ownerSocket = serviceRoot + "/data/manager/manager.sock"
const ctlSocket = runDir + "/ctl.sock"
const label = "org.zeroemployee.pomar.manager"
const plistPath = "/Library/LaunchDaemons/" + label + ".plist"
const archiveRoot = "/private/var/pomar-p-archive"

// Native is deliberately fixed to an already provisioned Pomar service. It
// cannot create a daemon, account, caller, class, image or signing identity.
type Native struct {
	M                        Manifest
	SHA, Bundle, ArchivePath string
	rollback                 bool
	fenced                   bool
	beforeRecord             []byte
	original                 Journal
	lock                     *os.File
}

func LoadNative(bundle, sha string) (*Native, error) {
	if err := privateRoot(bundle); err != nil {
		return nil, err
	}
	raw, err := readFile(filepath.Join(bundle, "upgrade.json"), 64<<10, 0)
	if err != nil {
		return nil, err
	}
	m, err := Decode(raw, sha)
	if err != nil {
		return nil, err
	}
	return &Native{M: m, SHA: sha, Bundle: bundle}, nil
}
func (n *Native) identity(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("existing service upgrade requires its administrator")
	}
	u, err := strconv.ParseUint(os.Getenv("SUDO_UID"), 10, 32)
	if err != nil || uint32(u) != n.M.OperatorUID {
		return fmt.Errorf("administrator identity differs from frozen manifest")
	}
	who, err := user.Lookup(os.Getenv("SUDO_USER"))
	if err != nil || who.Uid != strconv.FormatUint(u, 10) {
		return fmt.Errorf("sudo operator binding refused")
	}
	for name, uid := range map[string]uint32{"_pomar": n.M.OwnerUID, "_profrodci": n.M.CallerUID} {
		who, e := user.Lookup(name)
		if e != nil || who.Uid != strconv.FormatUint(uint64(uid), 10) {
			return fmt.Errorf("existing service account binding differs")
		}
	}
	host, err := command(ctx, nil, "/usr/sbin/scutil", "--get", "LocalHostName")
	if err != nil || strings.TrimSpace(string(host)) != n.M.Host {
		return fmt.Errorf("host binding differs")
	}
	return nil
}

type limitedOutput struct {
	bytes.Buffer
	max int
}

func (w *limitedOutput) Write(b []byte) (int, error) {
	if len(b) > w.max-w.Len() {
		return 0, fmt.Errorf("bounded command response exceeded")
	}
	return w.Buffer.Write(b)
}
func command(ctx context.Context, input []byte, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "TZ=UTC"}
	c.Dir = "/"
	c.Stdin = bytes.NewReader(input)
	out := &limitedOutput{max: 64 << 20}
	c.Stdout = out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("bounded command timed out")
		}
		return out.Bytes(), fmt.Errorf("checked %s operation refused", filepath.Base(path))
	}
	return out.Bytes(), nil
}
func owner(ctx context.Context, step string, extra ...string) ([]byte, error) {
	args := []string{"-n", "-u", "_pomar", "-H", bindir + "/pomar", "attempt", step, "-socket", ownerSocket}
	args = append(args, extra...)
	return command(ctx, nil, "/usr/bin/sudo", args...)
}
func retainedPath(name string) string {
	return map[string]string{"shim": bindir + "/pomar-shim-linux-arm64", "caller": bindir + "/pomar-ci-caller", "caller-policy": "/usr/local/etc/pomar/callers/65020.json", "caller-grant": "/private/etc/sudoers.d/pomar-profrod-caller", "classes": serviceRoot + "/config/profrod-classes-v1.json", "plist": plistPath}[name]
}
func (n *Native) retainedCheck() error {
	// The v1 product profile deliberately does not select arbitrary caller paths.
	if n.M.OwnerUID != 410 || n.M.CallerUID != 65020 {
		return fmt.Errorf("unsupported existing service profile")
	}
	for _, name := range retained {
		p := retainedPath(name)
		if err := directory(filepath.Dir(p), 0, 0); err != nil {
			return err
		}
		if _, err := pinnedFile(p, n.M.Retained[name]); err != nil {
			return fmt.Errorf("retained %s binding differs", name)
		}
	}
	return nil
}
func record(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || seen[k] {
			return nil, fmt.Errorf("ambiguous install record")
		}
		seen[k] = true
		out[k] = v
	}
	return out, nil
}
func (n *Native) targetSignature(ctx context.Context, path, name string) error {
	if n.M.SigningPolicy == "development_transition" {
		if err := developmentSignature(ctx, path); err != nil {
			return err
		}
	} else {
		id := "ac.zeo.pomar." + name
		requirement := fmt.Sprintf(`identifier "%s" and certificate leaf = H"%s"`, id, n.M.CertificateSHA1)
		if _, err := command(ctx, nil, "/usr/bin/codesign", "--verify", "--strict", "-R", requirement, path); err != nil {
			return fmt.Errorf("internally signed %s candidate required", name)
		}
	}
	if name == "pomar-host" {
		actual, err := command(ctx, nil, "/usr/bin/codesign", "--display", "--entitlements", ":-", path)
		if err != nil {
			return err
		}
		expected, err := pinnedFile(filepath.Join(n.Bundle, "pomar-host.entitlements"), n.M.Entitlements)
		if err != nil {
			return err
		}
		a, err := command(ctx, actual, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
		if err != nil {
			return err
		}
		b, err := command(ctx, expected, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
		if err != nil {
			return err
		}
		var av, bv map[string]any
		if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
			return fmt.Errorf("entitlements unreadable")
		}
		x, _ := json.Marshal(av)
		y, _ := json.Marshal(bv)
		if !bytes.Equal(x, y) || len(bv) != 1 || bv["com.apple.security.virtualization"] != true {
			return fmt.Errorf("entitlements mismatch")
		}
	} else {
		actual, err := command(ctx, nil, "/usr/bin/codesign", "--display", "--entitlements", ":-", path)
		if err != nil {
			return err
		}
		// The coordinator may not carry any entitlements.
		if len(bytes.TrimSpace(actual)) != 0 {
			a, err := command(ctx, actual, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
			if err != nil {
				return err
			}
			var v map[string]any
			if json.Unmarshal(a, &v) != nil || len(v) != 0 {
				return fmt.Errorf("coordinator entitlements refused")
			}
		}
	}
	return nil
}
func (n *Native) Check(ctx context.Context) error {
	if err := n.identity(ctx); err != nil {
		return err
	}
	if err := privateRoot(n.Bundle); err != nil {
		return err
	}
	raw, err := readFile(filepath.Join(n.Bundle, "upgrade.json"), 64<<10, 0)
	if err != nil || digest(raw) != n.SHA {
		return fmt.Errorf("frozen manifest changed")
	}
	for _, name := range binaries {
		if _, err := pinnedFile(filepath.Join(n.Bundle, name), n.M.Target.Binaries[name]); err != nil {
			return err
		}
		if err := n.targetSignature(ctx, filepath.Join(n.Bundle, name), name); err != nil {
			return err
		}
	}
	if err := n.retainedCheck(); err != nil {
		return err
	}
	for _, p := range []string{bindir, filepath.Dir(recordPath), archiveRoot} {
		if err := directory(p, 0, 0); err != nil {
			return err
		}
	}
	if err := directory(runDir, n.M.OwnerUID, 0); err != nil {
		return err
	}
	st, err := os.Lstat(runDir)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0750 && st.Mode().Perm() != 0700 {
		return fmt.Errorf("unexpected control directory mode")
	}
	raw, err = readFile(recordPath, 64<<10, 0)
	if err != nil {
		return err
	}
	r, err := record(raw)
	if err != nil {
		return err
	}
	if !n.rollback {
		if digest(raw) != n.M.InstallRecord.SHA256 || int64(len(raw)) != n.M.InstallRecord.Bytes || r["pin"] != n.M.Baseline.Pin || r["key_id"] != n.M.KeyID {
			return fmt.Errorf("installed baseline record differs")
		}
		hostPin := r["retained_host_pin"]
		if hostPin == "" {
			hostPin = r["pin"]
		}
		if hostPin != n.M.Baseline.HostPin {
			return fmt.Errorf("installed helper source binding differs")
		}
		n.beforeRecord = raw
	}
	for _, name := range binaries {
		b, err := readFile(bindir+"/"+name, 512<<20, 0)
		if err != nil {
			return err
		}
		old, next := n.M.Baseline.Binaries[name], n.M.Target.Binaries[name]
		if digest(b) != old.SHA256 || int64(len(b)) != old.Bytes {
			if !n.rollback || digest(b) != next.SHA256 || int64(len(b)) != next.Bytes {
				return fmt.Errorf("unknown installed %s bytes", name)
			}
		}
		if n.M.SigningPolicy == "development_transition" {
			if err := n.targetSignature(ctx, bindir+"/"+name, name); err != nil {
				return fmt.Errorf("development transition baseline signature differs: %w", err)
			}
		}
	}
	if n.rollback {
		_, err := n.recoveryObservation(ctx, n.original)
		return err
	}
	return nil
}

func developmentSignature(ctx context.Context, path string) error {
	if _, err := command(ctx, nil, "/usr/bin/codesign", "--verify", "--strict", path); err != nil {
		return fmt.Errorf("structurally valid development signature required")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "/usr/bin/codesign", "--display", "--verbose=4", path)
	c.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "TZ=UTC"}
	c.Dir = "/"
	info := &limitedOutput{max: 64 << 10}
	c.Stdout = io.Discard
	c.Stderr = info
	if err := c.Run(); err != nil {
		return fmt.Errorf("development signature metadata unavailable")
	}
	if !isDevelopmentSignature(info.String()) {
		return fmt.Errorf("explicit ad-hoc development transition required")
	}
	return nil
}

func isDevelopmentSignature(info string) bool {
	if !strings.Contains(info, "\nSignature=adhoc\n") {
		return false
	}
	for _, line := range strings.Split(info, "\n") {
		if !strings.HasPrefix(line, "CodeDirectory ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "flags=0x") {
				continue
			}
			bits, _, ok := strings.Cut(strings.TrimPrefix(field, "flags=0x"), "(")
			flags, err := strconv.ParseUint(bits, 16, 64)
			return ok && err == nil && flags&2 != 0
		}
	}
	return false
}

var pidPattern = regexp.MustCompile(`(?m)^\s*"PID"\s*=\s*([0-9]+);`)

func (n *Native) process(ctx context.Context) (int, string, error) {
	b, err := command(ctx, nil, "/bin/launchctl", "list", label)
	if err != nil {
		return 0, "", err
	}
	match := pidPattern.FindSubmatch(b)
	if len(match) != 2 {
		return 0, "", fmt.Errorf("service has no identified running process")
	}
	pid, _ := strconv.Atoi(string(match[1]))
	if pid <= 1 {
		return 0, "", fmt.Errorf("invalid service PID")
	}
	b, err = command(ctx, nil, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "uid=,lstart=,comm=")
	if err != nil {
		return 0, "", err
	}
	f := strings.Fields(string(b))
	if len(f) != 7 || f[0] != strconv.FormatUint(uint64(n.M.OwnerUID), 10) || f[6] != bindir+"/pomar" {
		return 0, "", fmt.Errorf("service process identity differs")
	}
	return pid, strings.Join(f[1:6], " "), nil
}
func canonical(raw []byte) ([]byte, error) {
	var v any
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &v) != nil {
		return nil, fmt.Errorf("invalid bounded observation")
	}
	return json.Marshal(v)
}
func history(raw []byte, class string) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || uniqueJSON(raw) != nil {
		return "", fmt.Errorf("ambiguous history")
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return "", fmt.Errorf("history not a list")
	}
	seen := map[string]bool{}
	var preserved []json.RawMessage
	for _, raw := range entries {
		var e manager.Entry
		if json.Unmarshal(raw, &e) != nil || e.Attempt == "" || seen[e.Attempt] || !e.Terminal() {
			return "", fmt.Errorf("live, duplicate or malformed attempt; upgrade refused")
		}
		if class != "" && e.Class.Name != class {
			return "", fmt.Errorf("caller history escaped fixed class")
		}
		seen[e.Attempt] = true
		b, err := canonical(raw)
		if err != nil {
			return "", err
		}
		preserved = append(preserved, b)
	}
	// Manager list is ordered; keep that order and every original field.
	b, _ := json.Marshal(preserved)
	return digest(b), nil
}
func key(raw []byte, expected string) error {
	var k manager.KeyReply
	if json.Unmarshal(raw, &k) != nil {
		return fmt.Errorf("key observation malformed")
	}
	pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil || len(pub) != 32 || k.Algorithm != "ed25519" || k.KeyID != expected || digest(pub) != expected {
		return fmt.Errorf("independently retained public key differs")
	}
	return nil
}
func capacity(raw []byte) (string, error) {
	var cp manager.Capacity
	if uniqueJSON(raw) != nil || json.Unmarshal(raw, &cp) != nil || cp.Live != 0 || cp.Class.Name == "" || cp.Class.VCPU < 1 || cp.Class.MemoryBytes < 1 || len(cp.Classes) == 0 || cp.Host.CPUSlots < 1 || cp.Host.MemoryBytes < 1 {
		return "", fmt.Errorf("non-idle or malformed capacity")
	}
	// Space, live count and drain are observations, not installation identity.
	b, _ := json.Marshal(struct {
		Class   any `json:"class"`
		Classes any `json:"classes"`
		Host    any `json:"host"`
	}{cp.Class, cp.Classes, cp.Host})
	return digest(b), nil
}
func (n *Native) caller(ctx context.Context, action string) ([]byte, error) {
	input, _ := json.Marshal(cicaller.Request{Action: action})
	b, err := command(ctx, input, "/usr/bin/sudo", "-n", "-u", "_profrodci", "-H", bindir+"/pomar-ci-caller")
	if err != nil {
		return nil, err
	}
	var r cicaller.Reply
	if uniqueJSON(b) != nil || json.Unmarshal(b, &r) != nil || r.CallerUID != int(n.M.CallerUID) || r.Status != 200 || r.Cached || !r.ResponseAvailable || r.Transport != "response" || r.Outcome != "snapshot" {
		return nil, fmt.Errorf("fresh actual caller readback refused")
	}
	return r.Body, nil
}
func (n *Native) Observe(ctx context.Context) (Observation, error) {
	var o Observation
	pid, birth, err := n.process(ctx)
	if err != nil {
		return o, err
	}
	raw, err := readFile(recordPath, 64<<10, 0)
	if err != nil {
		return o, err
	}
	r, err := record(raw)
	if err != nil {
		return o, err
	}
	o.Pin = r["pin"]
	b, err := owner(ctx, "signing-key")
	if err != nil {
		return o, err
	}
	if err = key(b, n.M.KeyID); err != nil {
		return o, err
	}
	o.KeyID = n.M.KeyID
	b, err = owner(ctx, "list")
	if err != nil {
		return o, err
	}
	o.History, err = history(b, "")
	if err != nil {
		return o, err
	}
	ownerHistory := append([]byte(nil), b...)
	b, err = owner(ctx, "vm-orphans")
	if err != nil {
		return o, err
	}
	var orphans []manager.VMOrphan
	if json.Unmarshal(b, &orphans) != nil {
		return o, fmt.Errorf("orphan observation malformed")
	}
	for _, v := range orphans {
		if !v.Gone {
			return o, fmt.Errorf("unresolved VM orphan")
		}
	}
	b, err = owner(ctx, "capacity")
	if err != nil {
		return o, err
	}
	ownerCap, err := capacity(b)
	if err != nil {
		return o, err
	}
	var initialCapacity manager.Capacity
	if json.Unmarshal(b, &initialCapacity) != nil {
		return o, fmt.Errorf("capacity malformed")
	}
	if initialCapacity.Draining && !n.fenced && !n.rollback {
		return o, fmt.Errorf("an existing owner drain must be reconciled before a new upgrade")
	}
	// Readbacks operate under the actual caller UID. They are GET-only and do
	// not create an attempt or a caller journal entry.
	policyBytes, err := pinnedFile(retainedPath("caller-policy"), n.M.Retained["caller-policy"])
	if err != nil {
		return o, err
	}
	var policy cicaller.Policy
	if json.Unmarshal(policyBytes, &policy) != nil || policy.CallerUID != int(n.M.CallerUID) || policy.ManagerUID != int(n.M.OwnerUID) || policy.Class == "" || policy.Socket != ctlSocket {
		return o, fmt.Errorf("caller policy binding differs")
	}
	b, err = n.caller(ctx, "list")
	if err != nil {
		return o, err
	}
	o.CallerHistory, err = history(b, policy.Class)
	if err != nil {
		return o, err
	}
	var entries []manager.Entry
	if json.Unmarshal(ownerHistory, &entries) != nil {
		return o, fmt.Errorf("history malformed")
	}
	for _, e := range entries {
		if e.Class.Name == policy.Class {
			continue
		}
		input, _ := json.Marshal(cicaller.Request{Action: "get", Attempt: e.Attempt})
		response, callErr := command(ctx, input, "/usr/bin/sudo", "-n", "-u", "_profrodci", "-H", bindir+"/pomar-ci-caller")
		var refused cicaller.Reply
		if callErr == nil || json.Unmarshal(response, &refused) != nil || refused.CallerUID != int(n.M.CallerUID) || refused.Status != 403 || !refused.ResponseAvailable || refused.Cached || refused.Transport != "response" || refused.Outcome != "refused" {
			return o, fmt.Errorf("actual legacy-class refusal not established")
		}
		break
	}
	b, err = n.caller(ctx, "signing-key")
	if err != nil {
		return o, err
	}
	if err = key(b, n.M.KeyID); err != nil {
		return o, err
	}
	b, err = n.caller(ctx, "capacity")
	if err != nil {
		return o, err
	}
	callerCap, err := capacity(b)
	if err != nil {
		return o, err
	}
	o.Capacity = digest([]byte(ownerCap + "\n" + callerCap))
	o.OwnerCapacity = ownerCap
	o.CallerCapacity = callerCap
	end, endBirth, err := n.process(ctx)
	if err != nil || end != pid || endBirth != birth {
		return o, fmt.Errorf("service changed during observation")
	}
	if err = n.noWriters(ctx, pid); err != nil {
		return o, err
	}
	o.PID = pid
	o.Birth = birth
	return o, nil
}

// Lock uses an advisory descriptor lock, not a stale PID pathname. check does
// not call this method. An interrupted archive remains available for rollback.
func (n *Native) Lock() error {
	if err := directory(serviceRoot, 0, 0711); err != nil {
		return err
	}
	fd, err := syscall.Open(serviceRoot+"/product-upgrade.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "product-upgrade.lock")
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	meta, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || meta.Uid != 0 || meta.Nlink != 1 || st.Mode().Perm() != 0600 {
		f.Close()
		return fmt.Errorf("upgrade lock custody refused")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("another product operation owns the service")
	}
	n.lock = f
	return nil
}
func (n *Native) Close() {
	if n.lock != nil {
		n.lock.Close()
		n.lock = nil
	}
}
func (n *Native) Archive(ctx context.Context, before Observation) (string, error) {
	if n.lock == nil {
		return "", fmt.Errorf("product operation lock required")
	}
	if err := directory(archiveRoot, 0, 0700); err != nil {
		return "", err
	}
	var space syscall.Statfs_t
	if err := syscall.Statfs(archiveRoot, &space); err != nil {
		return "", err
	}
	need := int64(256 << 20)
	for _, f := range n.M.Baseline.Binaries {
		need += f.Bytes
	}
	if uint64(space.Bavail)*uint64(space.Bsize) < uint64(need) {
		return "", fmt.Errorf("insufficient archive space")
	}
	path, err := os.MkdirTemp(archiveRoot, "product-")
	if err != nil {
		return "", err
	}
	n.ArchivePath = path
	if err = os.Chmod(path, 0700); err != nil {
		return path, err
	}
	if err = syncDirectory(archiveRoot); err != nil {
		return path, err
	}
	for _, name := range binaries {
		b, err := pinnedFile(bindir+"/"+name, n.M.Baseline.Binaries[name])
		if err != nil {
			return path, err
		}
		if err = durableWrite(filepath.Join(path, name), b, 0755); err != nil {
			return path, err
		}
	}
	b, err := pinnedFile(recordPath, n.M.InstallRecord)
	if err != nil {
		return path, err
	}
	n.beforeRecord = b
	if err = durableWrite(filepath.Join(path, "install-record"), b, 0600); err != nil {
		return path, err
	}
	raw, err := readFile(filepath.Join(n.Bundle, "upgrade.json"), 64<<10, 0)
	if err != nil || digest(raw) != n.SHA {
		return path, fmt.Errorf("manifest changed before archive")
	}
	if err = durableWrite(filepath.Join(path, "upgrade.json"), raw, 0600); err != nil {
		return path, err
	}
	return path, nil
}
func (n *Native) Save(j Journal) error {
	if n.lock == nil || j.Archive != n.ArchivePath || j.ManifestSHA != n.SHA {
		return fmt.Errorf("journal archive binding refused")
	}
	if err := privateRoot(n.ArchivePath); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return durableWrite(filepath.Join(n.ArchivePath, "journal.json"), append(b, '\n'), 0600)
}
func (n *Native) Fence(ctx context.Context) error {
	// Drain is atomic with admission. Existing clients cannot race an admitted
	// start past it; filesystem fencing follows only once the manager is idle.
	b, err := owner(ctx, "drain")
	if err != nil {
		if !n.rollback {
			return err
		}
		if err = n.absent(ctx); err != nil {
			return err
		}
		return n.closeControl()
	}
	var c manager.Capacity
	if json.Unmarshal(b, &c) != nil || !c.Draining || c.Live != 0 {
		return fmt.Errorf("drained owner is not idle")
	}
	n.fenced = true
	return nil
}
func (n *Native) closeControl() error {
	if err := directory(runDir, n.M.OwnerUID, 0); err != nil {
		return err
	}
	if err := os.Chmod(runDir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(ctlSocket)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	meta, ok := st.Sys().(*syscall.Stat_t)
	if !ok || st.Mode()&os.ModeSocket == 0 || meta.Uid != n.M.OwnerUID {
		return fmt.Errorf("control socket custody differs")
	}
	return os.Chmod(ctlSocket, 0600)
}
func (n *Native) noWriters(ctx context.Context, pid int) error {
	b, err := command(ctx, nil, "/bin/ps", "-axww", "-o", "pid=,ppid=,uid=,comm=")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 4 {
			return fmt.Errorf("incomplete process inventory")
		}
		p, e := strconv.Atoi(f[0])
		if e != nil {
			return fmt.Errorf("process inventory malformed")
		}
		pp, e := strconv.Atoi(f[1])
		if e != nil {
			return fmt.Errorf("process inventory malformed")
		}
		name := strings.Join(f[3:], " ")
		if (pid > 0 && pp == pid) || (f[2] == strconv.FormatUint(uint64(n.M.OwnerUID), 10) && p != pid && (name == bindir+"/pomar" || filepath.Base(name) == "pomar-host" || strings.Contains(name, "Virtualization"))) {
			return fmt.Errorf("service has surviving or unaccounted writers; no process was signalled")
		}
	}
	return nil
}
func (n *Native) Stop(ctx context.Context, before Observation) error {
	pid, birth, err := n.process(ctx)
	if err != nil || pid != before.PID || birth != before.Birth {
		return fmt.Errorf("matched service process changed before stop")
	}
	if err = n.noWriters(ctx, pid); err != nil {
		return err
	}
	b, err := owner(ctx, "capacity")
	if err != nil {
		return err
	}
	var cp manager.Capacity
	if json.Unmarshal(b, &cp) != nil || !cp.Draining || cp.Live != 0 {
		return fmt.Errorf("idle drain no longer confirmed")
	}
	if err = n.closeControl(); err != nil {
		return err
	}
	pid, birth, err = n.process(ctx)
	if err != nil || pid != before.PID || birth != before.Birth {
		return fmt.Errorf("process identity changed at stop")
	}
	if _, err = command(ctx, nil, "/bin/launchctl", "bootout", "system/"+label); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err = n.absent(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (n *Native) absent(ctx context.Context) error {
	// A failed label query is not absence. Require a successful bounded table
	// read, then independently prove that no owner writers remain.
	b, err := command(ctx, nil, "/bin/launchctl", "list")
	if err != nil {
		return err
	}
	if err = serviceAbsentFromTable(b); err != nil {
		return err
	}
	if err := n.noWriters(ctx, 0); err != nil {
		return err
	}
	return nil
}
func serviceAbsentFromTable(raw []byte) error {
	rows := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(rows) == 0 || strings.Join(strings.Fields(rows[0]), " ") != "PID Status Label" {
		return fmt.Errorf("launchd inventory incomplete")
	}
	for _, row := range rows[1:] {
		f := strings.Fields(row)
		if len(f) != 3 {
			return fmt.Errorf("launchd inventory malformed")
		}
		if f[2] == label {
			return fmt.Errorf("service is still loaded")
		}
	}
	return nil
}
func targetRecord(raw []byte, m Manifest, archive string) ([]byte, error) {
	r, err := record(raw)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, _, _ := strings.Cut(line, "=")
		if strings.HasPrefix(k, "prev_") || k == "pin" || k == "retained_host_pin" || k == "check_ok" || k == "reboot_ok" || k == "concurrency" || k == "validation" || k == "upgraded" || k == "sha256_pomar" || k == "sha256_pomar-host" || strings.HasPrefix(k, "cdhash_") {
			continue
		}
		lines = append(lines, line)
	}
	lines = append(lines, "pin="+m.Target.Pin, "retained_host_pin="+m.Target.HostPin, "sha256_pomar="+m.Target.Binaries["pomar"].SHA256, "sha256_pomar-host="+m.Target.Binaries["pomar-host"].SHA256, "prev_pin="+r["pin"], "prev_archive="+archive, "validation=product binary replacement; prior qualification invalidated; independent host qualification required")
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
func invalidatedRecord(raw []byte) ([]byte, error) {
	if _, err := record(raw); err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, _, _ := strings.Cut(line, "=")
		if k == "check_ok" || k == "reboot_ok" || k == "concurrency" || k == "validation" {
			continue
		}
		lines = append(lines, line)
	}
	return []byte(strings.Join(append(lines, "validation=product replacement in progress; prior qualification invalidated"), "\n") + "\n"), nil
}
func (n *Native) Replace(ctx context.Context) error {
	if n.lock == nil {
		return fmt.Errorf("operation lock missing")
	}
	if err := n.absent(ctx); err != nil {
		return err
	}
	if err := n.retainedCheck(); err != nil {
		return err
	}
	if _, err := pinnedFile(recordPath, n.M.InstallRecord); err != nil {
		return err
	}
	pending, err := invalidatedRecord(n.beforeRecord)
	if err != nil {
		return err
	}
	if err = durableWrite(recordPath, pending, 0644); err != nil {
		return err
	}
	for _, name := range binaries {
		if _, err := pinnedFile(bindir+"/"+name, n.M.Baseline.Binaries[name]); err != nil {
			return err
		}
		b, err := pinnedFile(filepath.Join(n.Bundle, name), n.M.Target.Binaries[name])
		if err != nil {
			return err
		}
		if err = n.targetSignature(ctx, filepath.Join(n.Bundle, name), name); err != nil {
			return err
		}
		if err = durableWrite(bindir+"/"+name, b, 0755); err != nil {
			return err
		}
		if _, err = pinnedFile(bindir+"/"+name, n.M.Target.Binaries[name]); err != nil {
			return err
		}
	}
	b, err := targetRecord(n.beforeRecord, n.M, n.ArchivePath)
	if err != nil {
		return err
	}
	return durableWrite(recordPath, b, 0644)
}
func (n *Native) Start(ctx context.Context) error {
	if err := n.absent(ctx); err != nil {
		return err
	}
	if err := directory(runDir, n.M.OwnerUID, 0700); err != nil {
		return err
	}
	if err := n.retainedCheck(); err != nil {
		return err
	}
	if _, err := command(ctx, nil, "/bin/launchctl", "bootstrap", "system", plistPath); err != nil {
		return err
	}
	ready, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		if _, err := owner(ready, "signing-key"); err == nil {
			break
		}
		select {
		case <-ready.Done():
			return fmt.Errorf("manager readiness unconfirmed; admission remains fenced")
		case <-time.After(time.Second):
		}
	}
	// The replacement manager starts with fresh in-memory drain state. Keep
	// the directory private until its new drain is explicitly confirmed.
	b, err := owner(ctx, "drain")
	if err != nil {
		return err
	}
	var cp manager.Capacity
	if json.Unmarshal(b, &cp) != nil || !cp.Draining || cp.Live != 0 {
		return fmt.Errorf("new idle drain not confirmed")
	}
	return nil
}
func (n *Native) Verify(ctx context.Context, before Observation) (err error) {
	build := n.M.Target
	if n.rollback {
		build = n.M.Baseline
	}
	if err = n.retainedCheck(); err != nil {
		return err
	}
	for _, name := range binaries {
		if _, err = pinnedFile(bindir+"/"+name, build.Binaries[name]); err != nil {
			return err
		}
		if !n.rollback {
			if err = n.targetSignature(ctx, bindir+"/"+name, name); err != nil {
				return err
			}
		}
	}
	raw, err := readFile(recordPath, 64<<10, 0)
	if err != nil {
		return err
	}
	expected := n.beforeRecord
	if !n.rollback {
		expected, err = targetRecord(expected, n.M, n.ArchivePath)
		if err != nil {
			return err
		}
	}
	if !bytes.Equal(raw, expected) {
		return fmt.Errorf("installed record differs from expected transition")
	}
	b, err := owner(ctx, "capacity")
	if err != nil {
		return err
	}
	var cp manager.Capacity
	if json.Unmarshal(b, &cp) != nil || !cp.Draining || cp.Live != 0 {
		return fmt.Errorf("post-install admission drain differs")
	}
	// Allow actual caller GET readbacks under the verified global drain. A
	// failed verification recloses the directory; starts remain drain-refused.
	if err = os.Chmod(ctlSocket, 0660); err != nil {
		return err
	}
	if err = os.Chmod(runDir, 0750); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = n.closeControl()
		}
	}()
	after, err := n.Observe(ctx)
	if err != nil {
		return err
	}
	if after.Pin != build.Pin || after.History != before.History || after.KeyID != before.KeyID || after.Capacity != before.Capacity || after.CallerHistory != before.CallerHistory {
		return fmt.Errorf("post-install history/key/class/budget/caller identity differs")
	}
	if err = n.noWriters(ctx, after.PID); err != nil {
		return err
	}
	return nil
}
func (n *Native) Open(ctx context.Context) error {
	if err := n.retainedCheck(); err != nil {
		return err
	}
	if err := directory(runDir, n.M.OwnerUID, 0750); err != nil {
		return err
	}
	b, err := owner(ctx, "capacity")
	if err != nil {
		return err
	}
	var cp manager.Capacity
	if json.Unmarshal(b, &cp) != nil || !cp.Draining || cp.Live != 0 {
		return fmt.Errorf("verified drain required before opening")
	}
	b, err = owner(ctx, "drain", "-off")
	if err != nil {
		return err
	}
	if json.Unmarshal(b, &cp) != nil || cp.Draining {
		return fmt.Errorf("admission reopening not confirmed")
	}
	return nil
}
func (n *Native) Restore(ctx context.Context, j Journal) error {
	if !n.rollback || n.lock == nil || j.Archive != n.ArchivePath {
		return fmt.Errorf("original rollback binding required")
	}
	if pid, birth, err := n.process(ctx); err == nil {
		b, err := owner(ctx, "list")
		if err != nil {
			return err
		}
		h, err := history(b, "")
		if err != nil || h != j.Before.History {
			return fmt.Errorf("history changed since upgrade; rollback refused")
		}
		if err = n.Stop(ctx, Observation{PID: pid, Birth: birth}); err != nil {
			return err
		}
	} else if err = n.absent(ctx); err != nil {
		return err
	}
	if err := n.retainedCheck(); err != nil {
		return err
	}
	for _, name := range binaries {
		current, err := readFile(bindir+"/"+name, 512<<20, 0)
		if err != nil {
			return err
		}
		old, next := n.M.Baseline.Binaries[name], n.M.Target.Binaries[name]
		if (digest(current) != old.SHA256 || int64(len(current)) != old.Bytes) && (digest(current) != next.SHA256 || int64(len(current)) != next.Bytes) {
			return fmt.Errorf("rollback refuses unknown binary bytes")
		}
		b, err := pinnedFile(filepath.Join(n.ArchivePath, name), old)
		if err != nil {
			return err
		}
		if err = durableWrite(bindir+"/"+name, b, 0755); err != nil {
			return err
		}
	}
	return durableWrite(recordPath, n.beforeRecord, 0644)
}
func (n *Native) LoadRollback(path string) (Journal, error) {
	var j Journal
	if filepath.Dir(path) != archiveRoot || !strings.HasPrefix(filepath.Base(path), "product-") {
		return j, fmt.Errorf("original product archive required")
	}
	if err := privateRoot(path); err != nil {
		return j, err
	}
	b, err := readFile(filepath.Join(path, "upgrade.json"), 64<<10, 0)
	if err != nil || digest(b) != n.SHA {
		return j, fmt.Errorf("archive manifest differs")
	}
	b, err = readFile(filepath.Join(path, "journal.json"), 64<<10, 0)
	if err != nil {
		return j, err
	}
	if uniqueJSON(b) != nil || json.Unmarshal(b, &j) != nil || j.Archive != path || j.ManifestSHA != n.SHA || j.Schema != "pomar.service-upgrade-journal/v1" {
		return j, fmt.Errorf("archive journal binding differs")
	}
	if j.Before.Pin != n.M.Baseline.Pin || j.Before.KeyID != n.M.KeyID || !hex64.MatchString(j.Before.History) || !hex64.MatchString(j.Before.Capacity) || !hex64.MatchString(j.Before.OwnerCapacity) || !hex64.MatchString(j.Before.CallerCapacity) || !hex64.MatchString(j.Before.CallerHistory) || j.Before.PID <= 1 || j.Before.Birth == "" {
		return j, fmt.Errorf("archive original observation binding differs")
	}
	n.beforeRecord, err = pinnedFile(filepath.Join(path, "install-record"), n.M.InstallRecord)
	if err != nil {
		return j, err
	}
	raw, err := readFile(recordPath, 64<<10, 0)
	if err != nil {
		return j, err
	}
	target, err := targetRecord(n.beforeRecord, n.M, path)
	if err != nil {
		return j, err
	}
	pending, err := invalidatedRecord(n.beforeRecord)
	if err != nil {
		return j, err
	}
	if !bytes.Equal(raw, n.beforeRecord) && !bytes.Equal(raw, target) && !bytes.Equal(raw, pending) {
		return j, fmt.Errorf("rollback refuses unknown install record")
	}
	n.ArchivePath = path
	n.rollback = true
	n.original = j
	return j, nil
}

func (n *Native) recoveryObservation(ctx context.Context, j Journal) (any, error) {
	pid, birth, err := n.process(ctx)
	if err != nil {
		if err = n.absent(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"schema": "pomar.service-recovery-check/v1", "read_only": true, "service": "absent", "journal": j}, nil
	}
	if err = n.noWriters(ctx, pid); err != nil {
		return nil, err
	}
	b, err := owner(ctx, "list")
	if err != nil {
		return nil, err
	}
	h, err := history(b, "")
	if err != nil || h != j.Before.History {
		return nil, fmt.Errorf("history changed since original operation")
	}
	b, err = owner(ctx, "signing-key")
	if err != nil {
		return nil, err
	}
	if err = key(b, j.Before.KeyID); err != nil {
		return nil, err
	}
	b, err = owner(ctx, "capacity")
	if err != nil {
		return nil, err
	}
	cap, err := capacity(b)
	if err != nil || cap != j.Before.OwnerCapacity {
		return nil, fmt.Errorf("owner class/budget changed since original operation")
	}
	end, endBirth, err := n.process(ctx)
	if err != nil || pid != end || birth != endBirth {
		return nil, fmt.Errorf("service changed during recovery check")
	}
	var cp manager.Capacity
	if json.Unmarshal(b, &cp) != nil {
		return nil, fmt.Errorf("capacity malformed")
	}
	return map[string]any{"schema": "pomar.service-recovery-check/v1", "read_only": true, "service": "running", "pid": pid, "birth": birth, "draining": cp.Draining, "history_preserved": true, "key_preserved": true, "owner_capacity_preserved": true, "caller_readback_performed": false, "journal": j}, nil
}

// Stage converts only the four frozen payload files into a private root-owned
// stage. It neither creates a service nor touches the installed service state.
func Stage(ctx context.Context, bundle, sha string) (string, error) {
	if os.Geteuid() != 0 || !filepath.IsAbs(bundle) {
		return "", fmt.Errorf("administrator and explicit payload directory required")
	}
	st, err := os.Lstat(bundle)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("payload directory must not be a symlink")
	}
	// Incoming seat-owned bytes are untrusted until their separately retained
	// hash is verified. Only the resulting root-private copy is executable.
	raw, err := readFile(filepath.Join(bundle, "upgrade.json"), 64<<10, ^uint32(0))
	if err != nil {
		return "", err
	}
	m, err := Decode(raw, sha)
	if err != nil {
		return "", err
	}
	n := &Native{M: m}
	if err = n.identity(ctx); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp("/private/var/tmp", "pomar-upgrade-")
	if err != nil {
		return "", err
	}
	if err = os.Chmod(stage, 0700); err != nil {
		return stage, err
	}
	if err = syncDirectory("/private/var/tmp"); err != nil {
		return stage, err
	}
	n.Bundle = stage
	n.SHA = sha
	if err = durableWrite(filepath.Join(stage, "upgrade.json"), raw, 0600); err != nil {
		return stage, err
	}
	pins := map[string]FilePin{"pomar": m.Target.Binaries["pomar"], "pomar-host": m.Target.Binaries["pomar-host"], "pomar-host.entitlements": m.Entitlements}
	for name, pin := range pins {
		b, err := readFile(filepath.Join(bundle, name), pin.Bytes, ^uint32(0))
		if err != nil || int64(len(b)) != pin.Bytes || digest(b) != pin.SHA256 {
			return stage, fmt.Errorf("staged payload binding refused")
		}
		mode := os.FileMode(0600)
		if name != "pomar-host.entitlements" {
			mode = 0700
		}
		if err = durableWrite(filepath.Join(stage, name), b, mode); err != nil {
			return stage, err
		}
	}
	for _, name := range binaries {
		if err = n.targetSignature(ctx, filepath.Join(stage, name), name); err != nil {
			return stage, err
		}
	}
	return stage, nil
}
