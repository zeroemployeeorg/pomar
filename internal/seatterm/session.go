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
func Attach(stream Stream, c Command, cols, rows uint16) error {
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
		// The PTY is always drained, even once the stream can't take more:
		// a terminal whose output nobody reads keeps its last process from
		// exiting (darwin waits for pending output on the last close).
		discard := false
		for {
			n, err := master.Read(buf)
			if n > 0 && !discard && send(Data, buf[:n]) != nil {
				discard = true
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
	case <-clientDone:
		// The operator left, or sent something malformed: the stream takes no
		// more (so a pending write is released and the PTY keeps draining),
		// then hang up the attach client's own group only, and give it a
		// moment before killing it.
		stream.SetWriteDeadline(time.Now())
		syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
		select {
		case err = <-exited:
		case <-time.After(3 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-exited
		}
	}
	// The teardown is bounded on every side. The remaining screen bytes and
	// the close go out within DrainTimeout: the PTY read and the stream
	// write both have deadlines, so a peer that stopped reading can't hold
	// Attach. Then the stream's reader is released.
	end := time.Now().Add(DrainTimeout)
	master.SetReadDeadline(end)
	stream.SetWriteDeadline(end)
	<-screenDone
	send(Close, nil)
	stream.SetReadDeadline(time.Now())
	<-clientDone
	return err
}

// DrainTimeout bounds an attach's teardown: its last screen bytes and its
// close frame.
const DrainTimeout = 2 * time.Second

// Stream is what an attach is carried over: a connection whose reads and
// writes can be bounded, as a net.Conn's can.
type Stream interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}
