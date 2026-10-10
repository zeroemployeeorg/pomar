package seatterm

import (
	"errors"
	"fmt"
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
	// Every side of the teardown is bounded by a deadline, so a stream or
	// terminal that can't take one is refused before anything starts.
	var zero time.Time
	if err := errors.Join(stream.SetReadDeadline(zero), stream.SetWriteDeadline(zero), master.SetReadDeadline(zero), master.SetWriteDeadline(zero)); err != nil {
		return fmt.Errorf("attach needs deadlines on its stream and terminal: %w", err)
	}
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
	var inputErr error
	input := make(chan []byte, InputQueue)
	inputDone := make(chan struct{})
	go func() {
		// The input pump: the terminal takes keys at its own pace, and never
		// holds up the stream's reader below.
		defer close(inputDone)
		broken := false
		for p := range input {
			if !broken {
				if _, err := master.Write(p); err != nil {
					broken = true // released at teardown, or the terminal side is gone
				}
			}
		}
	}()
	go func() {
		// The stream's reader never blocks on the terminal, so a disconnect is
		// always seen: keys queue, bounded, and a full queue ends the attach.
		defer close(clientDone)
		defer close(input)
		for {
			kind, payload, err := ReadFrame(stream)
			if err != nil || kind == Close {
				return
			}
			switch kind {
			case Data:
				select {
				case input <- payload:
				default:
					inputErr = ErrInputStalled
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
		// The operator left, or sent something malformed, or the terminal
		// stopped taking keys: the stream takes no more (so a pending write is
		// released and the PTY keeps draining), then hang up the attach
		// client's own group only, and give it a moment before killing it.
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
	// Attach. Then the stream's reader and the input pump are released. Each
	// wait also has its own limit; one that passes it is an error, and the
	// caller's close of the stream releases what is left.
	end := time.Now().Add(DrainTimeout)
	terr := errors.Join(master.SetReadDeadline(end), stream.SetWriteDeadline(end))
	if !waitUntil(screenDone, end.Add(time.Second)) {
		terr = errors.Join(terr, errors.New("the screen wasn't released"))
	}
	// The close frame goes out only when its write is bounded: the write
	// deadline was set and the screen writer has let go of the stream. After
	// a refused deadline or a screen that wasn't released, a close could
	// wait on the stream, or on the writer's lock, forever; it is skipped,
	// and the caller's close of the stream ends the attach instead.
	if terr == nil && wmu.TryLock() {
		WriteFrame(stream, Close, nil)
		wmu.Unlock()
	}
	terr = errors.Join(terr, stream.SetReadDeadline(time.Now()), master.SetWriteDeadline(time.Now()))
	if !waitUntil(clientDone, time.Now().Add(DrainTimeout)) || !waitUntil(inputDone, time.Now().Add(DrainTimeout)) {
		terr = errors.Join(terr, errors.New("the stream's reader wasn't released"))
	}
	if terr != nil {
		return errors.Join(ErrTeardown, terr)
	}
	if inputErr != nil {
		return inputErr
	}
	return err
}

func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	select {
	case <-done:
		return true
	case <-time.After(time.Until(deadline)):
		return false
	}
}

// DrainTimeout bounds an attach's teardown: its last screen bytes and its
// close frame.
const DrainTimeout = 2 * time.Second

// InputQueue bounds the keys queued for a terminal that is slow to take
// them, in frames of at most MaxData: 256 KiB.
const InputQueue = 8

// Stream is what an attach is carried over: a connection whose reads and
// writes can be bounded, as a net.Conn's can. Attach refuses a stream whose
// deadlines can't be set, before it starts anything.
type Stream interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// ErrInputStalled ends an attach whose terminal stopped taking its input.
var ErrInputStalled = errors.New("the terminal stopped taking its input")

// ErrTeardown is returned when a side of the teardown couldn't be bounded;
// the caller must close the stream, which releases it.
var ErrTeardown = errors.New("the attach's teardown wasn't bounded")
