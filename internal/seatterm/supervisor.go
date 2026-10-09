package seatterm

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Supervisor is the program in a seat's tmux session (SOW 16 §3.1). It
// never starts the actor by itself: it shows an idle prompt, and starts
// Claude Code only when the operator asks, and never again after it exits
// until asked again. The first start records a fresh session ID and uses
// --session-id; every later start resumes that recorded ID, so a seat is
// one conversation, not "the newest one in this folder".
//
// A second actor is refused while the first lives: the actor holds an
// exclusive lock on StateDir/actor.lock through a descriptor it inherits,
// so the lock outlives a dead tmux server or supervisor for as long as the
// actor itself runs. A missing tmux server or process name is never taken
// as proof that the actor exited.
type Supervisor struct {
	Seat     string
	StateDir string   // private, on the seat's workspace disk
	Agent    string   // the pinned Claude Code executable
	Args     []string // fixed arguments before the session flag
	Env      []string
	Dir      string
	In       io.Reader
	Out      io.Writer
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ErrActorRunning is a refusal to start while the seat's actor lives.
var ErrActorRunning = errors.New("an actor is still running for this seat; not starting another")

type sessionRecord struct {
	SessionID string `json:"session_id"`
	CreatedAt string `json:"created_at"`
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// session returns the seat's recorded session ID, recording a fresh one
// exclusively on the first start. fresh reports whether it was just made.
func (s Supervisor) session() (id string, fresh bool, err error) {
	path := filepath.Join(s.StateDir, "session.json")
	if b, err := os.ReadFile(path); err == nil {
		var r sessionRecord
		if json.Unmarshal(b, &r) != nil || !uuidV4.MatchString(r.SessionID) {
			return "", false, errors.New("the recorded session is unreadable; inspect it, don't replace it")
		}
		return r.SessionID, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	r := sessionRecord{SessionID: newUUID(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	b, _ := json.Marshal(r)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return s.session() // another supervisor recorded it first
	} else if err != nil {
		return "", false, err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return r.SessionID, true, err
}

func (s Supervisor) record(event string) {
	f, err := os.OpenFile(filepath.Join(s.StateDir, "starts.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), event)
	f.Close()
}

// Start starts the actor once and waits for it, holding the actor lock
// through the actor's own descriptor. It refuses while another actor holds
// the lock.
func (s Supervisor) Start() error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(s.StateDir, "actor.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrActorRunning
	}
	id, fresh, err := s.session()
	if err != nil {
		return err
	}
	flag := "--resume"
	if fresh {
		flag = "--session-id"
	}
	args := append(append([]string{}, s.Args...), flag, id)
	cmd := exec.Command(s.Agent, args...)
	cmd.Env, cmd.Dir = s.Env, s.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{lock} // the actor holds the lock while it lives
	s.record(flag + " " + id)
	if err := cmd.Start(); err != nil {
		s.record("start failed")
		return err
	}
	err = cmd.Wait()
	s.record(fmt.Sprintf("exited %v", err))
	return err
}

// Run shows the idle prompt until its input ends. "r" starts or resumes
// the seat's one session; anything else leaves the seat idle. It never
// starts the actor without being asked.
func (s Supervisor) Run() error {
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	in := bufio.NewReader(s.In)
	for {
		id := "none yet"
		if b, err := os.ReadFile(filepath.Join(s.StateDir, "session.json")); err == nil {
			var r sessionRecord
			if json.Unmarshal(b, &r) == nil {
				id = r.SessionID
			}
		}
		fmt.Fprintf(s.Out, "\nPomar seat %s is idle. Claude Code session: %s.\nType r and Enter to start or resume it; nothing starts on its own.\n> ", s.Seat, id)
		line, err := in.ReadString('\n')
		if strings.TrimSpace(line) == "r" {
			if serr := s.Start(); errors.Is(serr, ErrActorRunning) {
				fmt.Fprintln(s.Out, ErrActorRunning.Error()+".")
			} else if serr != nil {
				fmt.Fprintf(s.Out, "Claude Code ended: %v\n", serr)
			}
		}
		if err != nil {
			return nil
		}
	}
}
