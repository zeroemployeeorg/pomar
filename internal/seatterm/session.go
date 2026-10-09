package seatterm

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Command is the one fixed program an attach runs on the terminal: in a
// seat's guest, the tmux client for the seat's session. The stream never
// chooses it.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
	UID  int // -1 keeps the caller's
	GID  int
}

// Attach runs the command on a new pseudo-terminal sized cols x rows, and
// carries the terminal over the stream until either ends. When the stream
// ends first, it hangs up only the command's own process group (the
// attach client), never anything the command started in another session,
// such as the tmux server and the actor in it. It returns once the command
// has exited.
func Attach(stream io.ReadWriter, c Command, cols, rows uint16) error {
	if c.Path == "" || cols < 1 || cols > MaxCols || rows < 1 || rows > MaxRows {
		return errors.New("attach needs its command and a bounded window size")
	}
	master, tty, err := openPTY()
	if err != nil {
		return err
	}
	defer master.Close()
	slave, err := os.OpenFile(tty, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err == nil {
		// The size is set on the terminal side: darwin refuses it on the
		// controlling side.
		if err = setSize(slave, cols, rows); err != nil {
			slave.Close()
		}
	}
	if err != nil {
		return err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if c.UID >= 0 && c.GID >= 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID)}
	}
	err = cmd.Start()
	slave.Close()
	if err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var wmu sync.Mutex
	send := func(kind byte, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return WriteFrame(stream, kind, b)
	}
	screenDone := make(chan struct{})
	go func() {
		defer close(screenDone)
		buf := make([]byte, MaxData)
		for {
			n, err := master.Read(buf)
			if n > 0 && send(Data, buf[:n]) != nil {
				return
			}
			if err != nil {
				return // EIO once the terminal side has no process left
			}
		}
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			kind, payload, err := ReadFrame(stream)
			if err != nil || kind == Close {
				return
			}
			switch kind {
			case Data:
				if _, err := master.Write(payload); err != nil {
					return
				}
			case Resize:
				resize(tty, uint16(payload[0])<<8|uint16(payload[1]), uint16(payload[2])<<8|uint16(payload[3]))
			}
		}
	}()

	select {
	case err = <-exited:
		// The remaining screen bytes go out before the close; a platform whose
		// controlling side never reports the end is bounded by a deadline.
		master.SetReadDeadline(time.Now().Add(time.Second))
		<-screenDone
	case <-clientDone:
		// The operator left, or sent something malformed: hang up the attach
		// client's own group only, then give it a moment before killing it.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
		select {
		case err = <-exited:
		case <-time.After(3 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-exited
		}
	}
	send(Close, nil)
	return err
}
