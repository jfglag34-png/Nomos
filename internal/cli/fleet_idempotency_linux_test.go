//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func triggerFixture(t *testing.T, dir string) *fleet {
	t.Helper()
	t.Setenv("WEDRA_TRIGGER_TOKEN", "test-secret")
	t.Setenv("WEDRA_ALERT_WEBHOOK", "")
	t.Setenv("WEDRA_TELEGRAM_TOKEN", "")
	return &fleet{cfg: fleetConfig{RunsDir: dir, Jobs: []fleetJob{{ID: "parser", File: "unused", duration: time.Second}}}, ctx: context.Background(), exe: "/bin/true", active: map[string]bool{}, slots: make(chan struct{}, 6)}
}
func triggerRequest(f *fleet, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/trigger/parser", nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.handler().ServeHTTP(w, r)
	return w
}
func TestTriggerConcurrentAndRestart(t *testing.T) {
	dir := t.TempDir()
	f := triggerFixture(t, dir)
	const count = 64
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); responses <- triggerRequest(f, "same-request") }()
	}
	wg.Wait()
	close(responses)
	f.wg.Wait()
	accepted := 0
	id := ""
	for w := range responses {
		if w.Code == 202 {
			accepted++
		} else if w.Code != 200 {
			t.Fatalf("response %d: %s", w.Code, w.Body)
		}
		var b map[string]interface{}
		if e := json.Unmarshal(w.Body.Bytes(), &b); e != nil {
			t.Fatal(e)
		}
		current := b["trigger_id"].(string)
		if id == "" {
			id = current
		}
		if id != current {
			t.Fatal("receipt identity changed")
		}
	}
	if accepted != 1 {
		t.Fatalf("admitted %d", accepted)
	}
	runs, e := os.ReadDir(filepath.Join(dir, "parser"))
	if e != nil || len(runs) != 1 {
		t.Fatalf("runs=%d error=%v", len(runs), e)
	}
	restarted := triggerFixture(t, dir)
	w := triggerRequest(restarted, "same-request")
	if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("claim did not survive restart")
	}
	if len(restarted.active) != 0 {
		t.Fatal("replay started work")
	}
}
func TestTriggerBusyDoesNotConsumeKey(t *testing.T) {
	f := triggerFixture(t, t.TempDir())
	f.active["parser"] = true
	if w := triggerRequest(f, "retry-me"); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	delete(f.active, "parser")
	if w := triggerRequest(f, "retry-me"); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	f.wg.Wait()
}
func TestTriggerValidationAndFailClosed(t *testing.T) {
	f := triggerFixture(t, t.TempDir())
	for _, key := range []string{"", " space", "bad key", string(make([]byte, 129))} {
		if w := triggerRequest(f, key); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	dir := filepath.Join(f.cfg.RunsDir, ".trigger-claims")
	os.Mkdir(dir, 0700)
	if w := triggerRequest(f, "corrupt"); w.Code != 202 {
		t.Fatal(w.Code)
	}
	f.wg.Wait()
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("claim count")
	}
	os.WriteFile(filepath.Join(dir, files[0].Name()), []byte("truncated"), 0600)
	if w := triggerRequest(f, "corrupt"); w.Code != 503 {
		t.Fatal("corrupt claim replayed", w.Code)
	}
}
func TestTriggerDiskFailureDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".trigger-claims"), []byte("not directory"), 0600)
	f := triggerFixture(t, dir)
	if w := triggerRequest(f, "disk-failure"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if len(f.active) != 0 {
		t.Fatal("started despite disk failure")
	}
}
func TestTriggerCrashWindowClaimIsNotRerun(t *testing.T) {
	f := triggerFixture(t, t.TempDir())
	if w := triggerRequest(f, "crash-window"); w.Code != 202 {
		t.Fatal(w.Code)
	}
	f.wg.Wait()
	// A persisted claim is the only recovery evidence, even if the run directory
	// is absent (crash before launch, or external effects with lost local state).
	os.RemoveAll(filepath.Join(f.cfg.RunsDir, "parser"))
	restarted := triggerFixture(t, f.cfg.RunsDir)
	if w := triggerRequest(restarted, "crash-window"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, e := os.Stat(filepath.Join(f.cfg.RunsDir, "parser")); !os.IsNotExist(e) {
		t.Fatal("uncertain work rerun")
	}
}
