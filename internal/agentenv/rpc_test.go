package agentenv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRPCInterleavesActorEventsAndIgnoresLateReplies(t *testing.T) {
	input, actorOutput := io.Pipe()
	actorInput, output := io.Pipe()
	defer input.Close()
	defer output.Close()
	observed := make(chan Message, 1)
	rpc := NewRPC(input, output, func(m Message) error { observed <- m; return nil })
	actorDone := make(chan struct{})
	go func() {
		defer close(actorDone)
		defer actorOutput.Close()
		scan := bufio.NewScanner(actorInput)
		var first json.RawMessage
		for scan.Scan() {
			var request Message
			if json.Unmarshal(scan.Bytes(), &request) != nil {
				return
			}
			if request.Method == "lost" {
				first = request.ID
				continue
			}
			enc := json.NewEncoder(actorOutput)
			enc.Encode(Message{ID: first, Result: json.RawMessage(`{"late":true}`)})
			enc.Encode(Message{Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn"}}`)})
			enc.Encode(Message{ID: request.ID, Result: json.RawMessage(`{"ok":true}`)})
			return
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := rpc.Call(ctx, "lost", map[string]string{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost call: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	response, err := rpc.Call(ctx2, "inspect", map[string]string{})
	if err != nil || string(response) != `{"ok":true}` {
		t.Fatalf("inspection: %s %v", response, err)
	}
	select {
	case m := <-observed:
		if m.Method != "turn/started" {
			t.Fatal("wrong notification")
		}
	case <-ctx2.Done():
		t.Fatal("lost actor event")
	}
	<-actorDone
}
