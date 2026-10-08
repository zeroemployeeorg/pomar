package claudeactor

import (
	"strings"
	"testing"
)

// A harness result, not a model judgement or an authenticated native run
// (POMAR-CC fork 2, r44 §3.3(b); the POMAR Codex's 6052252128): text an
// agent puts in a controller request's data, including a synthetic secret, is
// carried verbatim to the controller. The bridge binds and bounds the request;
// it does not inspect or filter its content. Treating it as untrusted keeps it
// from becoming authority, but is no confidentiality protection: a credential
// held in the guest can be forwarded this way, which is the stated limitation
// of the in-guest credential (r40 rev 2).
func TestControllerDataIsCarriedVerbatim(t *testing.T) {
	h, sock := startController(t, "inbox")
	secret := "pomar-canary-harness-0123456789abcdef"
	args := `{"capability":"inbox","data":"the file holds ` + secret + `"}`
	emitToolUse(t, h, "toolu_f1", ControllerToolName, args)
	ch := forwardAsync(sock, ControllerTool, args)
	id, r := waitControllerRequest(t, h.broker)
	if !strings.Contains(r.Data, secret) {
		t.Fatalf("the journaled request lost the text: %+v", r)
	}
	listed := controllerRequests(t, h.broker).Requests[id]
	if !strings.Contains(listed.Data, secret) {
		t.Fatal("the controller's view lost the text")
	}
	replyController(t, h.broker, id, r, "reply-1", "received", true)
	waitForward(t, ch)
}
