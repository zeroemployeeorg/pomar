package agentenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/goproxy"
	"github.com/zeroemployeeorg/pomar/internal/proc"
)

// HostConfig is supplied by the service owner, never by a guest. A dedicated
// development data root is required; CI manager state is not opened here.
type HostConfig struct {
	ControllerCapabilities []string                      `json:"controllerCapabilities,omitempty"`
	Root                   string                        `json:"root"`
	Helper                 string                        `json:"helper"`
	Store                  string                        `json:"store"`
	Base                   string                        `json:"base"`
	Kernel                 string                        `json:"kernel"`
	InitRef                string                        `json:"initRef"`
	InitDigest             string                        `json:"initDigest"`
	ImageRef               string                        `json:"imageRef"`
	ImageDigest            string                        `json:"imageDigest"`
	GuestBinary            string                        `json:"guestBinary"`
	ShimBinary             string                        `json:"shimBinary"`
	CodexArchive           string                        `json:"codexArchive"`
	CodexArchiveSHA256     string                        `json:"codexArchiveSHA256"`
	SourceBundle           string                        `json:"sourceBundle"`
	SourceSHA              string                        `json:"sourceSHA"`
	GoProxySocket          string                        `json:"goProxySocket"`
	AllowedHosts           []string                      `json:"allowedHosts"`
	MaxLive                int                           `json:"maxLive"`
	CPUs                   int                           `json:"cpus"`
	MemoryBytes            uint64                        `json:"memoryBytes"`
	Profiles               map[string]EnvironmentProfile `json:"profiles,omitempty"`
	// Agent is the coding agent: empty or "codex" (the default), or "claude";
	// CodexArchive is then that agent's pinned package. AgentVersion pins
	// Claude Code's version, and is required for it.
	Agent        string `json:"agent,omitempty"`
	AgentVersion string `json:"agentVersion,omitempty"`
}

// EnvironmentProfile selects project inputs supplied by the service owner.
// Creation accepts only a name; a controller cannot supply filesystem paths.
type EnvironmentProfile struct {
	ControllerCapabilities []string `json:"controllerCapabilities,omitempty"`
	Base                   string   `json:"base"`
	ImageRef               string   `json:"imageRef"`
	ImageDigest            string   `json:"imageDigest"`
	SourceBundle           string   `json:"sourceBundle"`
	SourceSHA              string   `json:"sourceSHA"`
	AllowedHosts           []string `json:"allowedHosts"`
	// Agent, AgentVersion and AgentArchive(SHA256) select a profile's own
	// coding agent and its pinned package; empty keeps the host's.
	Agent              string `json:"agent,omitempty"`
	AgentVersion       string `json:"agentVersion,omitempty"`
	AgentArchive       string `json:"agentArchive,omitempty"`
	AgentArchiveSHA256 string `json:"agentArchiveSHA256,omitempty"`
}

var agentVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}$`)

// claudeControllerVersions are the Claude Code versions whose controller
// bridge is qualified (internal/claudeactor, TestLiveControllerTool).
var claudeControllerVersions = map[string]bool{"2.1.280": true}

// ValidateAgent refuses an unknown agent, a Claude Code agent without a
// pinned version, a version on Codex, and controller capabilities with a
// Claude Code version whose controller bridge is not qualified.
func ValidateAgent(c HostConfig) error {
	switch c.Agent {
	case "", "codex":
		if c.AgentVersion != "" {
			return errors.New("an agent version applies to the claude agent only")
		}
	case "claude":
		if !agentVersion.MatchString(c.AgentVersion) {
			return errors.New("the claude agent needs a pinned numeric agentVersion")
		}
		if len(c.ControllerCapabilities) > 0 && !claudeControllerVersions[c.AgentVersion] {
			return errors.New("controller capabilities are not qualified for this claude agent version")
		}
	default:
		return fmt.Errorf("unknown agent %q", c.Agent)
	}
	return nil
}

func (h *Host) profileConfig(name string) (HostConfig, error) {
	c := h.config
	c.Profiles = nil // Other projects' configuration does not enter a VM spec.
	if name == "" {
		return c, nil
	}
	p, ok := h.config.Profiles[name]
	if !ok {
		return HostConfig{}, errors.New("unknown owner environment profile")
	}
	c.Base, c.ImageRef, c.ImageDigest = p.Base, p.ImageRef, p.ImageDigest
	c.SourceBundle, c.SourceSHA = p.SourceBundle, p.SourceSHA
	c.AllowedHosts = append([]string(nil), p.AllowedHosts...)
	c.ControllerCapabilities = append([]string(nil), p.ControllerCapabilities...)
	if p.Agent != "" {
		c.Agent, c.AgentVersion = p.Agent, p.AgentVersion
	}
	if p.AgentArchive != "" {
		c.CodexArchive, c.CodexArchiveSHA256 = p.AgentArchive, p.AgentArchiveSHA256
	}
	return c, nil
}

// VMSpec's JSON keys also name the Swift VM owner's Codable options.
type VMSpec struct {
	HostConfig
	Environment   string `json:"environment"`
	Profile       string `json:"profile,omitempty"`
	Session       string `json:"session"`
	Incarnation   string `json:"incarnation"`
	Directory     string `json:"directory"`
	Rootfs        string `json:"rootfs"`
	ControlSocket string `json:"controlSocket"`
	EgressSocket  string `json:"egressSocket"`
}

type Action struct {
	Kind                 string          `json:"kind"`
	Expected             string          `json:"expected_incarnation"`
	State                string          `json:"state"`
	Fence                *Fence          `json:"fence,omitempty"`
	Reconciliation       *Reconciliation `json:"reconciliation,omitempty"`
	Retirement           *Retirement     `json:"retirement,omitempty"`
	ResultingIncarnation string          `json:"resulting_incarnation,omitempty"`
}
type Fence struct {
	Incarnation           string `json:"incarnation"`
	ScopeID               string `json:"scope_id"`
	NetworkRevoked        bool   `json:"network_revoked"`
	AdapterAccessRevoked  bool   `json:"adapter_access_revoked"`
	WorkspaceWriteRevoked bool   `json:"workspace_write_revoked"`
	TerminationConfirmed  bool   `json:"termination_confirmed"`
	State                 string `json:"state"`
}
type Environment struct {
	operationMu   sync.Mutex
	Spec          VMSpec            `json:"spec"`
	Phase         string            `json:"phase"`
	Process       proc.Process      `json:"process"`
	Actions       map[string]Action `json:"actions"`
	LastFence     *Fence            `json:"last_fence,omitempty"`
	LaunchRevoked []string          `json:"launch_revoked,omitempty"`
	HelperExit    *HelperExit       `json:"helper_exit,omitempty"`
}

// HelperExit is supervisor-observed process departure, never a VM stop receipt.
type HelperExit struct {
	Incarnation string `json:"incarnation"`
	PID         int    `json:"pid"`
	ObservedAt  string `json:"observed_at"`
	ExitCode    int    `json:"exit_code"`
	Signal      int    `json:"signal"`
}

type Host struct {
	mu        sync.Mutex
	config    HostConfig
	lock      *os.File
	envs      map[string]*Environment
	gates     map[string]*Egress
	servers   map[string]*http.Server
	processes proc.Lister
	stopWait  time.Duration
	probe     func(context.Context, FenceProbeRequest) (FenceProbeEvidence, error)
	fatal     error
}

var environmentID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func OpenHost(config HostConfig) (*Host, error) {
	if err := ValidateControllerCapabilities(config.ControllerCapabilities); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(config.Root) || config.MaxLive < 1 || config.MaxLive > 3 || config.CPUs < 1 || config.CPUs > 4 || config.MemoryBytes < 512<<20 || config.MemoryBytes > 8<<30 {
		return nil, errors.New("invalid development resource configuration")
	}
	if err := ValidateAgent(config); err != nil {
		return nil, err
	}
	for name, p := range config.Profiles {
		if err := ValidateControllerCapabilities(p.ControllerCapabilities); err != nil {
			return nil, err
		}
		effective := config
		effective.ControllerCapabilities = p.ControllerCapabilities
		if p.Agent != "" {
			effective.Agent, effective.AgentVersion = p.Agent, p.AgentVersion
		}
		if err := ValidateAgent(effective); err != nil {
			return nil, fmt.Errorf("profile %s: %w", name, err)
		}
		if (p.AgentArchive == "") != (p.AgentArchiveSHA256 == "") || (p.AgentArchive != "" && (!filepath.IsAbs(p.AgentArchive) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p.AgentArchiveSHA256))) {
			return nil, fmt.Errorf("profile %s: an agent archive needs an absolute path and its sha256", name)
		}
		if !environmentID.MatchString(name) || !filepath.IsAbs(p.Base) || !filepath.IsAbs(p.SourceBundle) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p.SourceSHA) || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(p.ImageDigest) || !strings.HasSuffix(p.ImageRef, "@"+p.ImageDigest) {
			return nil, errors.New("invalid owner environment profile")
		}
		if _, err := NewEgress(p.AllowedHosts); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(config.Root, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(config.Root)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("host data root must be private and not redirected")
	}
	lock, err := os.OpenFile(filepath.Join(config.Root, "host.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("agent host already owned")
	}
	h := &Host{config: config, lock: lock, envs: map[string]*Environment{}, gates: map[string]*Egress{}, servers: map[string]*http.Server{}}
	files, err := filepath.Glob(filepath.Join(config.Root, "*", "environment.json"))
	if err != nil {
		h.Close()
		return nil, err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			h.Close()
			return nil, err
		}
		var e Environment
		if err = json.Unmarshal(data, &e); err != nil {
			h.Close()
			return nil, err
		}
		if !validID.MatchString(e.Spec.Environment) || e.Spec.Directory != filepath.Join(config.Root, e.Spec.Environment) || e.Actions == nil {
			h.Close()
			return nil, errors.New("invalid retained environment identity")
		}
		h.envs[e.Spec.Environment] = &e
		if e.Phase == "running" {
			if !h.alive(&e) {
				e.Phase = "execution_unknown"
				h.save(&e)
				continue
			}
			if err = h.gate(&e); err != nil {
				h.Close()
				return nil, err
			}
		}
	}
	return h, nil
}

func (h *Host) Close() error {
	for _, g := range h.gates {
		g.Revoke()
	}
	for _, s := range h.servers {
		s.Close()
	}
	return h.lock.Close()
}
func (h *Host) save(e *Environment) (err error) {
	defer func() {
		if err != nil {
			h.fatal = err
		}
	}()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(e.Spec.Directory, ".environment-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(e.Spec.Directory, "environment.json")); err != nil {
		return err
	}
	d, err := os.Open(e.Spec.Directory)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (h *Host) alive(e *Environment) bool {
	ps, err := h.processList()
	if err != nil {
		return false
	}
	p, ok := proc.Find(ps, e.Process.PID)
	return ok && p.Start == e.Process.Start && p.UID == os.Getuid() && strings.Contains(p.Args, e.Spec.Helper) && strings.Contains(p.Args, filepath.Join(e.Spec.Directory, "vm-config-"+e.Spec.Incarnation+".json"))
}

func (h *Host) departed(e *Environment) bool {
	ps, err := h.processList()
	if err != nil {
		return false
	}
	p, ok := proc.Find(ps, e.Process.PID)
	return !ok || p.Start != e.Process.Start
}
func (h *Host) processList() ([]proc.Process, error) {
	if h.processes != nil {
		return h.processes.List()
	}
	return (proc.PS{}).List()
}

func (h *Host) gate(e *Environment) error {
	g, err := NewEgress(e.Spec.AllowedHosts)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(e.Spec.EgressSocket); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return errors.New("egress path occupied")
		}
		if err = os.Remove(e.Spec.EgressSocket); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", e.Spec.EgressSocket)
	if err != nil {
		return err
	}
	if err = os.Chmod(e.Spec.EgressSocket, 0o600); err != nil {
		ln.Close()
		return err
	}
	s := &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	h.gates[e.Spec.Environment] = g
	h.servers[e.Spec.Environment] = s
	go s.Serve(ln)
	cache := filepath.Join(e.Spec.Directory, "gocache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return err
	}
	goSocket := filepath.Join(e.Spec.Directory, "go.sock")
	if fi, err := os.Lstat(goSocket); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return errors.New("Go proxy path occupied")
		}
		if err = os.Remove(goSocket); err != nil {
			return err
		}
	}
	gl, err := net.Listen("unix", goSocket)
	if err != nil {
		return err
	}
	if err = os.Chmod(goSocket, 0o600); err != nil {
		gl.Close()
		return err
	}
	gp := &goproxy.Proxy{Cache: cache}
	goHandler := gp.Handler(io.Discard)
	gs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		revoked := g.revoked
		g.mu.Unlock()
		if revoked {
			http.Error(w, "egress revoked", 403)
			return
		}
		goHandler.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ConnState: func(c net.Conn, state http.ConnState) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if state == http.StateClosed {
			delete(g.connections, c)
			return
		}
		if g.revoked {
			c.Close()
			return
		}
		g.connections[c] = struct{}{}
	}}
	h.servers[e.Spec.Environment+"-go"] = gs
	go gs.Serve(gl)
	e.Spec.GoProxySocket = goSocket
	return nil
}

func (h *Host) start(e *Environment) error {
	if h.fatal != nil {
		return errors.New("owner journal durability uncertain; launch refused")
	}
	if e.Phase != "created" && e.Phase != "stopped" && e.Phase != "fenced" {
		return errors.New("environment is not confirmed stopped")
	}
	live := 0
	for _, other := range h.envs {
		if other.Phase != "stopped" && other.Phase != "created" && other.Phase != "fenced" && other.Phase != "retired" {
			live++
		}
	}
	if live >= h.config.MaxLive {
		return errors.New("development environment limit reached")
	}
	cfg, err := h.profileConfig(e.Spec.Profile)
	if err != nil {
		return err
	}
	if cfg.SourceSHA != e.Spec.SourceSHA || cfg.SourceBundle != e.Spec.SourceBundle || cfg.Base != e.Spec.Base || cfg.ImageDigest != e.Spec.ImageDigest || cfg.ImageRef != e.Spec.ImageRef {
		return errors.New("retained project inputs changed; use a distinct environment")
	}
	e.Spec.Incarnation = randomID()
	e.HelperExit = nil
	// Adopt the current owner's immutable artifacts only after the previous
	// execution scope has been fenced. Historical vm-config files stay intact.
	e.Spec.HostConfig = cfg
	e.Phase = "launching"
	if err := h.save(e); err != nil {
		return err
	}
	if err := h.gate(e); err != nil {
		return err
	}
	// A confirmed stopped helper is the only reason to remove the old marker.
	if err := os.Remove(filepath.Join(e.Spec.Directory, "stop")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, _ := json.Marshal(e.Spec)
	file := filepath.Join(e.Spec.Directory, "vm-config-"+e.Spec.Incarnation+".json")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(e.Spec.Directory, "helper.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(h.config.Helper, "agent-environment", "--config", file)
	cmd.Stdout = log
	cmd.Stderr = log
	// Explicit child environment: no host credential environment inheritance.
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + e.Spec.Directory}
	if err = cmd.Start(); err != nil {
		return err
	}
	incarnation := e.Spec.Incarnation
	go func() {
		cmd.Wait()
		h.mu.Lock()
		defer h.mu.Unlock()
		if e.Spec.Incarnation != incarnation {
			return
		}
		if cmd.ProcessState == nil {
			e.Phase = "execution_unknown"
			h.save(e)
			return
		}
		status, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
		e.HelperExit = &HelperExit{Incarnation: incarnation, PID: cmd.Process.Pid, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), ExitCode: cmd.ProcessState.ExitCode(), Signal: int(status.Signal())}
		if !status.Signaled() {
			e.HelperExit.Signal = 0
		}
		if e.Phase == "running" {
			e.Phase = "execution_unknown"
			if g := h.gates[e.Spec.Environment]; g != nil {
				g.Revoke()
			}
		}
		h.save(e)
	}()
	ps, err := (proc.PS{}).List()
	if err != nil {
		return err
	}
	p, ok := proc.Find(ps, cmd.Process.Pid)
	if !ok {
		return errors.New("helper departure unknown after launch")
	}
	e.Process = p
	e.Phase = "running"
	return h.save(e)
}

// stop revokes scoped network execution first. It never creates a replacement
// unless a matching VM-owner receipt and exact process departure are observed.
// The caller holds h.mu and this environment's operationMu. Release the shared
// lock while waiting so inspection and unrelated environments remain available.
func (h *Host) stop(e *Environment) error {
	if e.Phase == "stopped" || e.Phase == "fenced" || e.Phase == "retired" {
		return nil
	}
	if g := h.gates[e.Spec.Environment]; g != nil {
		g.Revoke()
	}
	e.Phase = "revoking"
	e.LastFence = &Fence{Incarnation: e.Spec.Incarnation, ScopeID: "scope-" + e.Spec.Incarnation, NetworkRevoked: true, AdapterAccessRevoked: true, State: "execution_unknown"}
	if err := h.save(e); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(e.Spec.Directory, "stop"), []byte(e.Spec.Incarnation), 0o600); err != nil {
		return err
	}
	wait := h.stopWait
	if wait == 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	h.mu.Unlock()
	defer h.mu.Lock()
	for time.Now().Before(deadline) {
		data, err := readVMStatus(e)
		var status map[string]string
		if err == nil && json.Unmarshal(data, &status) == nil && status["incarnation"] == e.Spec.Incarnation && status["vm_stopped"] == "true" && h.departed(e) {
			h.mu.Lock()
			e.Phase = "stopped"
			e.LastFence.WorkspaceWriteRevoked = true
			e.LastFence.TerminationConfirmed = true
			e.LastFence.State = "confirmed"
			if s := h.servers[e.Spec.Environment]; s != nil {
				s.Close()
			}
			if s := h.servers[e.Spec.Environment+"-go"]; s != nil {
				s.Close()
			}
			err := h.save(e)
			h.mu.Unlock()
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.mu.Lock()
	e.Phase = "execution_unknown"
	h.save(e)
	h.mu.Unlock()
	return errors.New("network revoked but VM execution departure unconfirmed; replacement refused")
}

func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments/{id}/retire", h.retireHandler)
	mux.HandleFunc("POST /v1/environments/{id}/reconcile", h.reconcileHandler)
	mux.HandleFunc("POST /v1/environments/{id}/continuations", h.continuationHandler)
	mux.HandleFunc("POST /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID          string `json:"id"`
			OperationID string `json:"operation_id"`
			Profile     string `json:"profile,omitempty"`
		}
		if !decode(w, r, &req) {
			return
		}
		if !environmentID.MatchString(req.ID) || !validID.MatchString(req.OperationID) {
			respond(w, 400, map[string]string{"error": "invalid identifiers"})
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if existing := h.envs[req.ID]; existing != nil {
			action, ok := existing.Actions[req.OperationID]
			if !ok || action.Kind != "create" || existing.Spec.Profile != req.Profile {
				failure(w, ErrConflict)
				return
			}
			respond(w, 200, existing)
			return
		}
		cfg, err := h.profileConfig(req.Profile)
		if err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		dir := filepath.Join(h.config.Root, req.ID)
		if err := os.Mkdir(dir, 0o700); err != nil {
			failure(w, err)
			return
		}
		e := &Environment{Spec: VMSpec{HostConfig: cfg, Profile: req.Profile, Environment: req.ID, Session: "session-" + randomID(), Incarnation: "created-" + randomID(), Directory: dir, Rootfs: filepath.Join(dir, "workspace.ext4"), ControlSocket: filepath.Join(dir, "agent.sock"), EgressSocket: filepath.Join(dir, "egress.sock")}, Phase: "created", Actions: map[string]Action{req.OperationID: {Kind: "create", State: "completed"}}}
		if err := h.save(e); err != nil {
			failure(w, err)
			return
		}
		h.envs[req.ID] = e
		respond(w, 201, e)
	})
	mux.HandleFunc("GET /v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		e := h.envs[r.PathValue("id")]
		if e == nil {
			respond(w, 404, map[string]string{"error": "unknown environment"})
			return
		}
		respond(w, 200, map[string]any{"environment": e, "helper_alive": h.alive(e)})
	})
	mux.HandleFunc("GET /v1/environments/{id}/operations/{operation}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		e := h.envs[r.PathValue("id")]
		if e == nil {
			respond(w, 404, map[string]string{"evidence": "unknown", "error": "environment unavailable"})
			return
		}
		if r.URL.Query().Get("session_id") != e.Spec.Session || r.URL.Query().Get("expected_incarnation") == "" {
			respond(w, 409, map[string]string{"evidence": "unknown", "error": "original identity required"})
			return
		}
		incarnation := r.URL.Query().Get("expected_incarnation")
		a, ok := e.Actions[r.PathValue("operation")]
		if !ok {
			evidence := "unknown"
			if incarnation == e.Spec.Incarnation {
				evidence = "durable_non_acceptance"
			}
			respond(w, 404, map[string]string{"evidence": evidence})
			return
		}
		if a.Expected != incarnation {
			failure(w, ErrConflict)
			return
		}
		respond(w, 200, map[string]any{"operation_id": r.PathValue("operation"), "session_id": e.Spec.Session, "action": a})
	})
	mux.HandleFunc("POST /v1/environments/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			OperationID string `json:"operation_id"`
			Incarnation string `json:"expected_incarnation"`
		}
		if !decode(w, r, &req) {
			return
		}
		kind := r.PathValue("action")
		if kind != "start" && kind != "stop" && kind != "replace" {
			respond(w, 404, map[string]string{"error": "unknown action"})
			return
		}
		h.mu.Lock()
		e := h.envs[r.PathValue("id")]
		if e == nil {
			h.mu.Unlock()
			respond(w, 404, map[string]string{"error": "unknown environment"})
			return
		}
		h.mu.Unlock()
		e.operationMu.Lock()
		defer e.operationMu.Unlock()
		h.mu.Lock()
		defer h.mu.Unlock()
		if existing, ok := e.Actions[req.OperationID]; ok {
			if existing.Kind != kind || existing.Expected != req.Incarnation {
				failure(w, ErrConflict)
				return
			}
			respond(w, 200, e)
			return
		}
		if req.Incarnation != e.Spec.Incarnation {
			failure(w, ErrStale)
			return
		}
		if !validID.MatchString(req.OperationID) {
			failure(w, errors.New("invalid operation id"))
			return
		}
		e.Actions[req.OperationID] = Action{Kind: kind, Expected: req.Incarnation, State: "dispatching"}
		if err := h.save(e); err != nil {
			failure(w, err)
			return
		}
		var err error
		switch kind {
		case "start":
			err = h.start(e)
		case "stop":
			err = h.stop(e)
		case "replace":
			if err = h.stop(e); err == nil {
				err = h.start(e)
			}
		}
		a := e.Actions[req.OperationID]
		a.State = "completed"
		if err != nil {
			a.State = "acceptance_unknown"
		}
		if kind == "stop" || kind == "replace" {
			a.Fence = e.LastFence
		}
		if err == nil && (kind == "start" || kind == "replace") {
			a.ResultingIncarnation = e.Spec.Incarnation
		}
		e.Actions[req.OperationID] = a
		if saveErr := h.save(e); saveErr != nil {
			failure(w, saveErr)
			return
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, e)
	})
	mux.HandleFunc("/v1/environments/{id}/agent/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("path") == "continuations" {
			respond(w, http.StatusForbidden, map[string]string{"error": "continuation requires the machine-owned host binding route"})
			return
		}
		h.mu.Lock()
		e := h.envs[r.PathValue("id")]
		if e == nil || e.Phase != "running" {
			h.mu.Unlock()
			failure(w, errors.New("environment not running; inspect execution state"))
			return
		}
		socket := e.Spec.ControlSocket
		h.mu.Unlock()
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}
		defer transport.CloseIdleConnections()
		proxy := &httputil.ReverseProxy{Transport: transport, Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "guest"
			req.URL.Path = "/v1/" + r.PathValue("path")
			req.Host = "guest"
		}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			respond(w, 502, map[string]string{"error": "actor unavailable; inspect operation acceptance before retrying"})
		}}
		proxy.ServeHTTP(w, r)
	})
	return mux
}

// Serve owns only this development socket. Reconnection opens a retained
// service/guest; it does not restart the agent or resubmit a task.
func (h *Host) Serve(ctx context.Context) error {
	path := filepath.Join(h.config.Root, "host.sock")
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return errors.New("host socket path occupied")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(path, 0o600); err != nil {
		return err
	}
	server := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	go func() { <-ctx.Done(); server.Close() }()
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ReadHostConfig never creates or reads a credential store.
func ReadHostConfig(path string) (HostConfig, error) {
	var c HostConfig
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&c)
	return c, err
}

func (h *Host) String() string { return fmt.Sprintf("agent development host %s", h.config.Root) }
