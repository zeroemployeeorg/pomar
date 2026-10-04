package agentenv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// RPCError exports only the numeric protocol code, never server message/data
// bytes which may contain account or provider details.
type RPCError struct{ Code int }

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex RPC error code %d; inspect retained operation state", e.Code)
}

// RPC keeps the actor connection alive independently of any controller HTTP
// request. Only the guest broker can write to the actor's stdin.
type RPC struct {
	mu      sync.Mutex
	w       io.Writer
	next    uint64
	wait    map[string]chan Message
	done    chan struct{}
	err     error
	observe func(Message) error
}

func NewRPC(reader io.Reader, writer io.Writer, observe func(Message) error) *RPC {
	r := &RPC{w: writer, wait: map[string]chan Message{}, done: make(chan struct{}), observe: observe}
	go r.read(reader)
	return r
}

func (r *RPC) read(reader io.Reader) {
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 4096), 2<<20)
	var err error
	for s.Scan() {
		var m Message
		if err = json.Unmarshal(s.Bytes(), &m); err != nil {
			break
		}
		if m.Method == "" && len(m.ID) > 0 {
			r.mu.Lock()
			ch := r.wait[string(m.ID)]
			r.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
			continue
		}
		if r.observe != nil {
			if err = r.observe(m); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = s.Err()
	}
	if err == nil {
		err = io.EOF
	}
	r.mu.Lock()
	r.err = err
	close(r.done)
	r.mu.Unlock()
}

func (r *RPC) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = r.w.Write(b)
	return err
}

func (r *RPC) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return nil, err
	}
	r.next++
	id := fmt.Sprintf("pomar-%d", r.next)
	raw, _ := json.Marshal(id)
	ch := make(chan Message, 1)
	r.wait[string(raw)] = ch
	err := r.write(map[string]any{"id": id, "method": method, "params": params})
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.wait, string(raw)); r.mu.Unlock() }()
	if err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if len(m.Error) > 0 {
			var failure struct {
				Code int `json:"code"`
			}
			if json.Unmarshal(m.Error, &failure) != nil {
				return nil, errors.New("codex RPC returned a malformed error; inspect retained operation state")
			}
			return nil, &RPCError{Code: failure.Code}
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, errors.New("actor connection ended; dispatch acceptance may be unknown")
	}
}

func (r *RPC) Notify(method string, params any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.write(map[string]any{"method": method, "params": params})
}
func (r *RPC) Answer(id json.RawMessage, result any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	return r.write(map[string]any{"id": id, "result": result})
}
func (r *RPC) Done() <-chan struct{} { return r.done }
