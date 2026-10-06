package agentenv

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in native qualification uses an existing native sign-in, never reads or
// copies credential stores, and creates an isolated read-only ephemeral thread.
// This test is not a fixture result or a Linux VM / recipient admission proof.
type qualificationActor struct{ Actor }

func (a qualificationActor) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "thread/start" {
		p := params.(map[string]any)
		p["ephemeral"] = true
		p["sandbox"] = "read-only"
		p["approvalPolicy"] = "never"
	}
	return a.Actor.Call(ctx, method, params)
}

func TestControllerRealNativeExchange(t *testing.T) {
	binary := os.Getenv("POMAR_NATIVE_CODEX")
	if binary == "" {
		t.Skip("set POMAR_NATIVE_CODEX to the already installed pinned binary for real native qualification")
	}
	workspace := t.TempDir()
	b := NewBroker(newStore(t), workspace)
	if evidence := os.Getenv("POMAR_NATIVE_EVIDENCE_DIR"); evidence != "" {
		t.Cleanup(func() {
			raw, err := json.MarshalIndent(b.Store.Snapshot(), "", "  ")
			if err != nil {
				t.Error(err)
				return
			}
			if err = os.WriteFile(filepath.Join(evidence, "native-session.json"), raw, 0600); err != nil {
				t.Error(err)
			}
		})
	}
	if err := b.ConfigureController([]string{"echo"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "app-server", "--listen", "stdio://", "-c", "analytics.enabled=false")
	cmd.Dir = workspace
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Errorf("owned qualification process %d has not exited after stdin EOF", cmd.Process.Pid)
		}
	})
	actor := qualificationActor{NewRPC(out, in, b.Observe)}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err = b.Initialize(ctx, actor); err != nil {
		t.Fatal(err)
	}
	task := Task{OperationID: "native-controller-qualification", Incarnation: "actor-one", Text: "Use only the pomar_controller_request dynamic tool. Invoke capability echo with data qualification-request. Do not run any command, read/write files, use network tools or invoke other tools. After the controller replies, output its reply text verbatim and finish."}
	if _, err = b.Submit(task); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(150 * time.Second)
	nonce := "pomar-native-reply-" + randomID()
	replied := false
	var record ControllerRequest
	for time.Now().Before(deadline) {
		state := b.Store.Snapshot()
		if !replied {
			for _, request := range state.ControllerRequests {
				if request.Capability != "echo" || request.Data != "qualification-request" {
					t.Fatal("unexpected native request")
				}
				reply := ControllerReply{ReplyID: "native-reply", Binding: request.Binding, Text: nonce, Success: true}
				raw, _ := json.Marshal(reply)
				w := httptest.NewRecorder()
				b.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/controller/requests/"+request.ID+"/reply", strings.NewReader(string(raw))))
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &record) != nil || record.State != "written" {
					t.Fatalf("native response: %d %s", w.Code, w.Body.String())
				}
				replied = true
				t.Logf("real native request bound: operation=%s input=%s session=%s incarnation=%s thread=%s turn=%s request=%s call=%s reply_sha256=%s", request.Binding.OperationID, request.Binding.InputSHA256, request.Binding.SessionID, request.Binding.Incarnation, request.Binding.ThreadID, request.Binding.TurnID, request.Binding.NativeRequestID, request.Binding.CallID, record.ReplySHA256)
			}
		}
		if op := state.Operations[task.OperationID]; op.Completion != "" {
			var nativeCompletion, modelEcho bool
			var deltas strings.Builder
			for _, event := range state.Events {
				if event.Method == "item/completed" {
					var p struct {
						Item struct {
							Type         string `json:"type"`
							ID           string `json:"id"`
							Success      bool   `json:"success"`
							ContentItems []struct {
								Text string `json:"text"`
							} `json:"contentItems"`
						} `json:"item"`
					}
					json.Unmarshal(event.Params, &p)
					if p.Item.Type == "dynamicToolCall" && p.Item.ID == record.Binding.CallID && p.Item.Success && len(p.Item.ContentItems) == 1 && p.Item.ContentItems[0].Text == nonce {
						nativeCompletion = true
					}
				}
				if event.Method == "item/agentMessage/delta" && strings.Contains(string(event.Params), nonce) {
					modelEcho = true
				}
				if event.Method == "item/agentMessage/delta" {
					var p struct {
						ThreadID string `json:"threadId"`
						TurnID   string `json:"turnId"`
						Delta    string `json:"delta"`
					}
					json.Unmarshal(event.Params, &p)
					if p.ThreadID == record.Binding.ThreadID && p.TurnID == record.Binding.TurnID {
						deltas.WriteString(p.Delta)
					}
				}
				if event.Method == "item/completed" {
					var p struct {
						ThreadID string `json:"threadId"`
						TurnID   string `json:"turnId"`
						Item     struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"item"`
					}
					json.Unmarshal(event.Params, &p)
					if p.ThreadID == record.Binding.ThreadID && p.TurnID == record.Binding.TurnID && p.Item.Type == "agentMessage" && strings.Contains(p.Item.Text, nonce) {
						modelEcho = true
					}
				}
			}
			modelEcho = modelEcho || strings.Contains(deltas.String(), nonce)
			if !replied || op.Completion != "completed" || !nativeCompletion || !modelEcho {
				t.Fatalf("real native exchange incomplete: reply=%v terminal=%s native_completion=%v model_echo=%v", replied, op.Completion, nativeCompletion, modelEcho)
			}
			t.Log("PASS: native tool completion and requesting actor's fresh nonce echo; transport delivery is not organisational acknowledgement")
			return
		}
		select {
		case <-actor.Done():
			t.Fatal("native adapter exited before qualification completed")
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("bounded native qualification timed out; native acceptance remains unresolved")
}
