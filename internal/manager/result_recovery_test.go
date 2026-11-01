package manager

// sow: pomar-SOW-87. Synthetic persisted boundary fixtures. These tests exercise
// the real reader/publication paths; they are not host power-loss qualification.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/zeroemployeeorg/pomar/internal/cache"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/venue"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/result"
)

func publicationRequest(m *Manager, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/v1/attempts/"+id+"/result", nil)
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{known: true, uid: uint32(m.cfg.UID)}))
	w := httptest.NewRecorder()
	m.mux(true).ServeHTTP(w, r)
	return w
}

func prepareCompletion(t *testing.T, m *Manager, id string) (Entry, string) {
	t.Helper()
	e := terminalEntry(id)
	e.ResultPolicy = m.resultPolicy()
	dir := filepath.Join(m.cfg.Venue.Root(), attemptsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m.freezeResult(&e)
	m.t.entries[id] = &e
	if err := m.t.save(); err != nil {
		t.Fatal(err)
	}
	e.completionSaved = true
	return e, dir
}

func TestCompletionIntentSurvivesReloadAndKeyOutageWithoutRerun(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, _ := prepareCompletion(t, m, "original-failed")
	// Record a genuine failure; a publication retry must preserve it.
	e.State = StateFailed
	code := 7
	e.ExitCode = &code
	e.Publication = Publication{}
	m.freezeResult(&e)
	m.t.entries[e.Attempt] = &e
	if err := m.t.save(); err != nil {
		t.Fatal(err)
	}
	frozen := bytes.Clone(e.Publication.Intent.Document)
	loaded, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	m.t = loaded
	signer := m.cfg.Signer
	m.cfg.Signer = nil
	now := time.Now()
	m.recoverResults(now)
	w := publicationRequest(m, e.Attempt)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || !bytes.Contains(w.Body.Bytes(), []byte("key_unavailable")) {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := m.Remove(e.Attempt); err == nil {
		t.Fatal("unresolved evidence removed")
	}
	// A rotated key cannot silently change the frozen host or re-sign its claim.
	other, _, _ := openSigning(t, true)
	m.cfg.Signer = other.cfg.Signer
	m.recoverResults(now.Add(2 * time.Second))
	if m.t.entries[e.Attempt].Publication.Error != "key_unavailable" {
		t.Fatal("rotated key accepted")
	}
	m.cfg.Signer = signer
	m.recoverResults(now.Add(10 * time.Second))
	w = publicationRequest(m, e.Attempt)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	doc, sig, err := result.Read(filepath.Join(m.cfg.Venue.Root(), attemptsDir, e.Attempt))
	if err != nil || !bytes.Equal(doc, frozen) || !result.Verify(signer.Public(), doc, sig) {
		t.Fatal("original completion was not recovered", err)
	}
	if m.t.entries[e.Attempt].State != StateFailed || *m.t.entries[e.Attempt].ExitCode != 7 {
		t.Fatal("execution failure rewritten")
	}
	if m.t.entries[e.Attempt].Publication.State != "published" {
		t.Fatal("publication not persisted")
	}
}

func TestCompletionStorageRetryKeepsFrozenBytesAndBackoff(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, dir := prepareCompletion(t, m, "storage-retry")
	held := dir + "-held"
	if err := os.Rename(dir, held); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.recoverResults(now)
	p := m.t.entries[e.Attempt].Publication
	if p.State != "pending" || p.Error != "storage_unavailable" || p.NextRetry.Sub(now) != time.Second {
		t.Fatal(p)
	}
	if w := publicationRequest(m, e.Attempt); w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	m.recoverResults(now.Add(time.Millisecond))
	if m.t.entries[e.Attempt].Publication.Attempts != 1 {
		t.Fatal("retry ignored backoff")
	}
	if err := os.Rename(held, dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	m.t = loaded
	m.recoverResults(now.Add(2 * time.Second))
	if w := publicationRequest(m, e.Attempt); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	got, _, _ := result.Read(dir)
	if !bytes.Equal(got, e.Publication.Intent.Document) {
		t.Fatal("retry rebuilt document")
	}
}

func TestCompletionPartialAndCorruptEvidenceThroughPublicReader(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "document-only", true: "conflicting-signature"}[corrupt], func(t *testing.T) {
			m, _, _ := openSigning(t, true)
			e, dir := prepareCompletion(t, m, "partial")
			if err := result.Write(dir, e.Publication.Intent.Document, nil); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(filepath.Join(dir, result.SigName), []byte("malformed"), 0400); err != nil {
					t.Fatal(err)
				}
			}
			w := publicationRequest(m, e.Attempt)
			want := 503
			if corrupt {
				want = 409
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
			m.recoverResults(time.Now())
			w = publicationRequest(m, e.Attempt)
			if corrupt {
				if w.Code != 409 || m.t.entries[e.Attempt].Publication.State != "corrupt" {
					t.Fatal(w.Code, w.Body.String())
				}
				got, _ := os.ReadFile(filepath.Join(dir, result.SigName))
				if string(got) != "malformed" {
					t.Fatal("corrupt evidence rewritten")
				}
				if m.Remove(e.Attempt) == nil {
					t.Fatal("corrupt evidence removed")
				}
			} else if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestCompletionLegacyMissingAndMalformedSignaturesNeverHTTP200(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "malformed"}[malformed], func(t *testing.T) {
			m, _, _ := openSigning(t, true)
			e := terminalEntry("legacy")
			dir := filepath.Join(m.cfg.Venue.Root(), attemptsDir, e.Attempt)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			m.t.entries[e.Attempt] = &e
			doc, err := result.Canonical(m.resultDoc(e))
			if err != nil {
				t.Fatal(err)
			}
			var sig []byte
			if malformed {
				sig = []byte("not-an-ed25519-signature")
			}
			if err := result.Write(dir, doc, sig); err != nil {
				t.Fatal(err)
			}
			w := publicationRequest(m, e.Attempt)
			reason := "result_unknown"
			if malformed {
				reason = "result_corrupt"
			}
			if w.Code != 409 || !bytes.Contains(w.Body.Bytes(), []byte(reason)) {
				t.Fatal(w.Code, w.Body.String())
			}
			before := base64.StdEncoding.EncodeToString(doc)
			m.recoverResults(time.Now())
			got, _, _ := result.Read(dir)
			if base64.StdEncoding.EncodeToString(got) != before {
				t.Fatal("legacy evidence changed")
			}
		})
	}
}

func TestCompletionIntentConflictIsHeldNotSigned(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, _ := prepareCompletion(t, m, "wrong-intent")
	// Even a recomputed digest cannot authorize a document for a different ID.
	i := *e.Publication.Intent
	i.Document = bytes.Replace(i.Document, []byte("wrong-intent"), []byte("other-original"), 1)
	i.SHA256 = documentHash(i.Document)
	m.t.entries[e.Attempt].Publication.Intent = &i
	m.recoverResults(time.Now())
	if w := publicationRequest(m, e.Attempt); w.Code != http.StatusConflict {
		t.Fatal(w.Code, w.Body.String())
	}
	if m.t.entries[e.Attempt].Publication.State != "corrupt" {
		t.Fatal("invalid intent not durably held")
	}
}

func TestCompletionFailedTerminalTransactionDoesNotPublishOrDiscard(t *testing.T) {
	m, _, _ := openSigning(t, true)
	id := "terminal-save-failure"
	e := terminalEntry(id)
	e.State = StateRunning
	e.ExitCode = nil
	e.ResultPolicy = m.resultPolicy()
	dir := filepath.Join(m.cfg.Venue.Root(), attemptsDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m.t.entries[id] = &e
	if err := m.t.save(); err != nil {
		t.Fatal(err)
	}
	// A real open failure at the table's staging name models transient storage
	// refusal. It is not a simulated passing fsync or physical power-loss proof.
	if err := os.Mkdir(m.t.path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	code := 0
	m.finish(id, StateExited, "", &code)
	disk, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.entries[id].Terminal() {
		t.Fatal("failed transaction persisted terminal without intent")
	}
	if e.Publication.Intent == nil || e.completionSaved {
		t.Fatal("RAM completion lost or falsely durable")
	}
	if _, err := os.Stat(filepath.Join(dir, result.DocName)); !os.IsNotExist(err) {
		t.Fatal("published before terminal intent was durable")
	}
	if m.Remove(id) == nil {
		t.Fatal("failed transaction discarded evidence")
	}
	if err := os.Remove(m.t.path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	m.recoverResults(time.Now())
	if w := publicationRequest(m, id); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	disk, err = loadTable(m.t.path)
	if err != nil || disk.entries[id].Publication.State != "published" {
		t.Fatal("retry not durable", err)
	}
}

func TestCompletionRetentionKeepsPendingAndCorruptRecords(t *testing.T) {
	m, _, v := openSigning(t, true)
	m.cfg.Budgets = []cache.Budget{{Name: "attempt-records", Prefix: "attempts/", Bytes: 1, Evict: true, Class: venue.ClassAttempt}}
	for _, id := range []string{"keep-pending", "keep-corrupt"} {
		// Exercise the actual venue/retention ledger, not only the explicit remover.
		put(t, v, "volume", "attempt", "attempt-"+id, "attempts/"+id, 2*mib)
		e := terminalEntry(id)
		e.ResultPolicy = m.resultPolicy()
		m.freezeResult(&e)
		e.completionSaved = true
		if id == "keep-corrupt" {
			e.Publication.State = "corrupt"
			e.Publication.Error = "result_conflict"
		}
		m.t.entries[id] = &e
	}
	rep, err := m.Caches(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range rep.Evicted {
		if ev.Object.ID == "attempt-keep-pending" || ev.Object.ID == "attempt-keep-corrupt" {
			t.Fatal("unresolved record evicted")
		}
	}
	for _, id := range []string{"keep-pending", "keep-corrupt"} {
		if m.Remove(id) == nil {
			t.Fatal("unresolved record removed", id)
		}
	}
}

// A child exits at real persisted boundaries. Restart uses Open itself and its
// ordinary automatic finalizer. No controller, client, provider or build exists.
func TestCompletionProcessBoundary(t *testing.T) {
	stage := os.Getenv("POMAR_FINALIZER_FIXTURE_STAGE")
	if stage == "" {
		return
	}
	root := os.Getenv("POMAR_FINALIZER_FIXTURE_ROOT")
	v, err := openFixtureVenue(root)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := result.LoadOrCreate(filepath.Join(root, "keys"), os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Venue: v, HostBin: "/nonexistent-build-must-not-run", Procs: noProcs{}, UID: os.Getuid(), Signer: signer, Poll: time.Hour, Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}})
	if err != nil {
		t.Fatal(err)
	}
	e, dir := prepareCompletion(t, m, "persisted-original")
	doc := e.Publication.Intent.Document
	switch stage {
	case "durable-intent":
	case "partial-stage":
		if err := os.WriteFile(filepath.Join(dir, ".result-pending-fixture"), doc[:len(doc)/2], 0400); err != nil {
			t.Fatal(err)
		}
	case "synced-stage":
		f, err := os.OpenFile(filepath.Join(dir, ".result-pending-fixture"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(doc); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	case "document", "signature-staged", "pair":
		if err := result.Write(dir, doc, nil); err != nil {
			t.Fatal(err)
		}
		if stage == "signature-staged" {
			f, err := os.OpenFile(filepath.Join(dir, ".result-pending-signature"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(signer.Sign(doc)); err != nil {
				t.Fatal(err)
			}
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "pair" {
			if err := result.Publish(dir, doc, signer.Sign(doc)); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatal("unknown test stage")
	}
	// Deliberately omit manager Close: model process interruption, not shutdown.
	os.Exit(0)
}

func TestCompletionStartupRecoversOriginalAcrossProcessBoundaries(t *testing.T) {
	for _, stage := range []string{"durable-intent", "partial-stage", "synced-stage", "document", "signature-staged", "pair"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			v, err := openFixtureVenue(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Init(); err != nil {
				t.Fatal(err)
			}
			if err := v.EnsureCache(venue.KindVolume, "keys", "keys"); err != nil {
				t.Fatal(err)
			}
			signer, err := result.LoadOrCreate(filepath.Join(root, "keys"), os.Getuid())
			if err != nil {
				t.Fatal(err)
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestCompletionProcessBoundary$", "-test.count=1")
			cmd.Env = append(os.Environ(), "POMAR_FINALIZER_FIXTURE_STAGE="+stage, "POMAR_FINALIZER_FIXTURE_ROOT="+root)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture exit: %v %s", err, b)
			}
			before, err := loadTable(tablePath(filepath.Join(root, managerDir)))
			if err != nil {
				t.Fatal(err)
			}
			frozen := bytes.Clone(before.entries["persisted-original"].Publication.Intent.Document)
			m, err := Open(Config{Venue: v, HostBin: "/nonexistent-build-must-not-run", Procs: noProcs{}, UID: os.Getuid(), Signer: signer, Poll: time.Hour, Host: capacity.Host{CPUSlots: 4, MemoryBytes: 8 * capacity.GiB}})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if w := publicationRequest(m, "persisted-original"); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			doc, sig, err := result.Read(filepath.Join(root, attemptsDir, "persisted-original"))
			if err != nil || !bytes.Equal(doc, frozen) || !result.Verify(signer.Public(), doc, sig) {
				t.Fatal("startup did not recover exact original", err)
			}
			durable, err := loadTable(m.t.path)
			if err != nil || durable.entries["persisted-original"].Publication.State != "published" {
				t.Fatal("publication acknowledgment not durable", err)
			}
			if len(m.List()) != 1 {
				t.Fatal("new execution identity created")
			}
		})
	}
}

func TestCompletionCompletePairReconcilesWithoutPrivateSigner(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, dir := prepareCompletion(t, m, "pair-before-ack")
	if err := result.Publish(dir, e.Publication.Intent.Document, m.cfg.Signer.Sign(e.Publication.Intent.Document)); err != nil {
		t.Fatal(err)
	}
	m.cfg.Signer = nil
	m.recoverResults(time.Now())
	if m.t.entries[e.Attempt].Publication.State != "published" {
		t.Fatal("complete verified original incorrectly requires private key again")
	}
	if w := publicationRequest(m, e.Attempt); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompletionRoundRobinDoesNotStarveLaterOriginal(t *testing.T) {
	m, _, _ := openSigning(t, true)
	for index := range 20 {
		id := fmt.Sprintf("publication-%02d", index)
		e, _ := prepareCompletion(t, m, id)
		if index < 19 {
			m.recoverResults(time.Now())
		}
		_ = e
	}
	for range 3 {
		m.recoverResults(time.Now())
	}
	if w := publicationRequest(m, "publication-19"); w.Code != 200 {
		t.Fatal("later original starved", w.Code, w.Body.String())
	}
}

func TestCompletionRestartReacknowledgesIntentBeforePublication(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, dir := prepareCompletion(t, m, "rename-before-dir-sync")
	// Model a process cut after the fsynced table inode is renamed but BEFORE
	// its parent directory is synced. Visibility on process restart does not prove
	// that namespace transition survived a storage/power fault. No power-loss
	// claim is made by this fixture.
	body, err := json.Marshal(m.t.list())
	if err != nil {
		t.Fatal(err)
	}
	temp := m.t.path + ".before-dir-sync"
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, m.t.path); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	m.t = loaded
	// A real storage refusal prevents re-acknowledgment. Publication must remain
	// pending even though the newly renamed table was visible to the reader.
	if err := os.Mkdir(m.t.path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	m.recoverResults(time.Now())
	if w := publicationRequest(m, e.Attempt); w.Code != 503 {
		t.Fatalf("unacknowledged restart must stay pending, got%d:%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, result.DocName)); !os.IsNotExist(err) {
		t.Fatal("published before durable restart acknowledgment")
	}
	if err := os.Remove(m.t.path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	m.recoverResults(time.Now())
	if w := publicationRequest(m, e.Attempt); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPublishedCompletionRestartRequiresDurableReacknowledgment(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, _ := prepareCompletion(t, m, "published-restart-reack")
	m.recoverResults(time.Now())
	if w := publicationRequest(m, e.Attempt); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	loaded, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	m.t = loaded
	if err := os.Mkdir(m.t.path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	// Check the reader before and after the published branch attempts its save:
	// each guard is independently necessary, even for an already complete pair.
	if w := publicationRequest(m, e.Attempt); w.Code != 503 {
		t.Fatalf("reader before reack: %d %s", w.Code, w.Body.String())
	}
	m.recoverResults(time.Now())
	if w := publicationRequest(m, e.Attempt); w.Code != 503 {
		t.Fatalf("reader after refused reack: %d %s", w.Code, w.Body.String())
	}
	if m.resultDisposable(*m.t.entries[e.Attempt]) {
		t.Fatal("unacknowledged original became disposable")
	}
	if err := os.Remove(m.t.path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	m.recoverResults(time.Now())
	if w := publicationRequest(m, e.Attempt); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCompletionInRecordSymlinkIsCorruptAndRetained(t *testing.T) {
	for _, name := range []string{result.DocName, result.SigName} {
		t.Run(name, func(t *testing.T) {
			m, _, _ := openSigning(t, true)
			e, dir := prepareCompletion(t, m, "in-record-link")
			doc := e.Publication.Intent.Document
			if err := result.Publish(dir, doc, m.cfg.Signer.Sign(doc)); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, "planted")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("planted", filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			m.recoverResults(time.Now())
			if w := publicationRequest(m, e.Attempt); w.Code != 409 {
				t.Fatalf("symlink must be corrupt: %d %s", w.Code, w.Body.String())
			}
			retained := *m.t.entries[e.Attempt]
			if retained.Publication.State != "corrupt" || m.resultDisposable(retained) {
				t.Fatal("symlink released custody", retained.Publication.State)
			}
			if err := m.Remove(e.Attempt); err == nil {
				t.Fatal("corrupt evidence removed")
			}
		})
	}
}

func TestPublicationReadbackStorageFailureRemainsRetryable(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, dir := prepareCompletion(t, m, "readback-storage-refusal")
	m.recoverResults(time.Now())
	retained := m.t.entries[e.Attempt]
	if retained.Publication.State != "published" {
		t.Fatal(retained.Publication.State)
	}
	// Actual EACCES during the final publication readback, not conflicting bytes.
	// Run through the same classifier called before publication custody release.
	name := filepath.Join(dir, result.DocName)
	if err := os.Chmod(name, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(name, 0400)
	m.checkPublicationReadback(retained)
	if retained.Publication.State != "pending" || retained.Publication.Error != "storage_unavailable" {
		t.Fatalf("transient storage refusal became permanent: %s %s", retained.Publication.State, retained.Publication.Error)
	}
	if w := publicationRequest(m, e.Attempt); w.Code != 503 {
		t.Fatalf("storage failure must be public503: %d %s", w.Code, w.Body.String())
	}
	if m.resultDisposable(*retained) {
		t.Fatal("storage-inaccessible result became disposable")
	}
	if err := m.t.save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTable(m.t.path)
	if err != nil {
		t.Fatal(err)
	}
	m.t = loaded
	if err := os.Chmod(name, 0400); err != nil {
		t.Fatal(err)
	}
	// Already-complete original reconciles after restart without private signer.
	m.cfg.Signer = nil
	m.recoverResults(time.Now().Add(2 * time.Second))
	if w := publicationRequest(m, e.Attempt); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(m.List()) != 1 {
		t.Fatal("recovery created another execution")
	}
}

func TestPublicationReadbackConflictNeverBecomesRetryable(t *testing.T) {
	m, _, _ := openSigning(t, true)
	e, dir := prepareCompletion(t, m, "readback-corrupt-proof")
	m.recoverResults(time.Now())
	name := filepath.Join(dir, result.SigName)
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	bad := []byte("malformed retained signature")
	if err := os.WriteFile(name, bad, 0400); err != nil {
		t.Fatal(err)
	}
	retained := m.t.entries[e.Attempt]
	m.checkPublicationReadback(retained)
	if retained.Publication.State != "corrupt" {
		t.Fatal("conflicting proof became retryable", retained.Publication.State)
	}
	if w := publicationRequest(m, e.Attempt); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	m.recoverResults(time.Now().Add(time.Minute))
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, bad) {
		t.Fatal("corrupt proof overwritten", err)
	}
	if m.resultDisposable(*retained) {
		t.Fatal("corrupt proof released custody")
	}
}
