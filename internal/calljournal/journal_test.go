package calljournal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCrashedPublicationRetainsAndRecoversSameOneTimeEvent(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	event := Event{ID: "event-00000000001", Kind: "response", At: time.Now().UTC().Format(time.RFC3339Nano), Value: json.RawMessage(`{"one_time":"FIXTURE-ONLY"}`)}
	b, _ := json.Marshal(event)
	pending := filepath.Join(dir, ".pending-crash")
	os.WriteFile(pending, b, 0600)
	// Both before link and after link/before unlink are real crash boundaries.
	for _, linked := range []bool{false, true} {
		if linked {
			os.WriteFile(pending, b, 0600)
			os.Link(pending, filepath.Join(dir, event.ID+".json"))
		}
		if e := Recover(dir, func(e Event) bool { return e.Kind == "response" && bytes.Equal(e.Value, event.Value) }); e != nil {
			t.Fatal(e)
		}
		got, ok, e := Latest(dir)
		if e != nil || !ok || got.ID != event.ID || !bytes.Equal(got.Value, event.Value) {
			t.Fatal(got, e)
		}
		fi, e := os.Stat(filepath.Join(dir, event.ID+".json"))
		if e != nil || fi.Mode().Perm() != 0600 {
			t.Fatal(e)
		}
		if !linked {
			os.Remove(filepath.Join(dir, event.ID+".json"))
		}
	}
}
func TestPartialOrWrongBindingIsNeverDeletedOrPromoted(t *testing.T) {
	for _, b := range [][]byte{[]byte(`{"event_id":`), []byte(`{"event_id":"event-00000000001","kind":"response","observed_at":"2026-10-08T00:00:00Z","value":{"wrong":true}}`)} {
		dir := t.TempDir()
		pending := filepath.Join(dir, ".pending-original")
		os.WriteFile(pending, b, 0600)
		if e := Recover(dir, func(Event) bool { return false }); e == nil {
			t.Fatal("unproven staged response accepted")
		}
		after, e := os.ReadFile(pending)
		if e != nil || !bytes.Equal(after, b) {
			t.Fatal("unique partial evidence removed")
		}
	}
}
