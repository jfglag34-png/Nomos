//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func durableJob(id string) fleetJob {
	return fleetJob{ID: id, File: "unused", Timeout: "1s", duration: time.Second}
}
func testStore(t *testing.T, dir string, limit int) *fleetStore {
	t.Helper()
	s, e := openFleetStore(dir, limit, []string{"webhook", "telegram"})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestDurableConcurrentAdmissionAndRestart(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, dir, 100)
	var wg sync.WaitGroup
	var fresh atomic.Int32
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, replay, e := s.enqueue(durableJob("p"), "same")
			if e != nil {
				t.Error(e)
			}
			if !replay {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 {
		t.Fatal(fresh.Load())
	}
	restarted := testStore(t, dir, 100)
	task, replay, e := restarted.enqueue(durableJob("p"), "same")
	if e != nil || !replay || task.Status != "pending" {
		t.Fatal(task, replay, e)
	}
	running, e := restarted.take(map[string]bool{})
	if e != nil || running == nil {
		t.Fatal(e)
	}
	afterCrash := testStore(t, dir, 100)
	task, replay, e = afterCrash.enqueue(durableJob("p"), "same")
	if e != nil || !replay || task.Status != "interrupted" {
		t.Fatal(task, replay, e)
	}
	if n := len(afterCrash.state.Notices); n != 2 {
		t.Fatalf("recovery notices %d", n)
	}
	// Restarting again must not emit the same interruption a second time.
	again := testStore(t, dir, 100)
	if len(again.state.Notices) != 2 {
		t.Fatal("duplicate recovery notice")
	}
}
func TestDurableQueueLimitAndPerJobSerialization(t *testing.T) {
	s := testStore(t, t.TempDir(), 2)
	first, _, _ := s.enqueue(durableJob("p"), "first")
	s.enqueue(durableJob("p"), "second")
	if _, _, e := s.enqueue(durableJob("q"), "third"); e != errFleetQueueFull {
		t.Fatal(e)
	}
	if _, replay, e := s.enqueue(durableJob("p"), "first"); e != nil || !replay {
		t.Fatal("replay rejected at capacity")
	}
	task, e := s.take(map[string]bool{})
	if e != nil || task.ID != first.ID {
		t.Fatal("not FIFO")
	}
	if task, e = s.take(map[string]bool{"p": true}); e != nil || task != nil {
		t.Fatal("parallel same job")
	}
	s.enqueue(durableJob("q"), "third")
	if task, e = s.take(map[string]bool{"p": true}); e != nil || task == nil || task.Job.ID != "q" {
		t.Fatal("head of line blocked unrelated job")
	}
}
func TestDurableIntervalCheckpoint(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, dir, 10)
	j := durableJob("interval")
	j.interval = 7 * time.Minute
	j.Every = "7m"
	now := time.Now().UTC()
	if e := s.scheduleInterval(j, now); e != nil {
		t.Fatal(e)
	}
	re := testStore(t, dir, 10)
	if e := re.scheduleInterval(j, now.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if len(re.state.Tasks) != 1 {
		t.Fatal("interval replay after restart")
	}
	if e := re.scheduleInterval(j, now.Add(8*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if len(re.state.Tasks) != 1 {
		t.Fatal("scheduled pending task not coalesced")
	}
}
func TestDurableCompletionAndOutboxSameSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, dir, 10)
	task, _, _ := s.enqueue(durableJob("p"), "one")
	s.take(map[string]bool{})
	if e := s.finish(task.ID, "success"); e != nil {
		t.Fatal(e)
	}
	re := testStore(t, dir, 10)
	if re.state.Tasks[task.ID].Status != "success" || len(re.state.Notices) != 2 {
		t.Fatal("completion/outbox not atomic")
	}
	for _, n := range re.state.Notices {
		if n.Event != "success" {
			t.Fatal("completed task treated as interrupted")
		}
	}
}
func TestDurableOutboxRetryAfterRestart(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing event key")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	t.Setenv("WEDRA_ALERT_WEBHOOK", server.URL)
	dir := t.TempDir()
	s := testStore(t, dir, 10)
	if e := s.notify("success", "p", "done"); e != nil {
		t.Fatal(e)
	}
	n := s.noticeBatch("webhook", time.Now())[0]
	if deliverFleetNotice(context.Background(), n) {
		t.Fatal("503 considered delivered")
	}
	if e := s.noticeResult(n.ID, false); e != nil {
		t.Fatal(e)
	}
	re := testStore(t, dir, 10)
	if re.state.Notices[n.ID].Attempts != 1 || re.state.Notices[n.ID].Delivered {
		t.Fatal("retry lost")
	}
	if len(re.noticeBatch("webhook", time.Now())) != 0 {
		t.Fatal("backoff ignored")
	}
	batch := re.noticeBatch("webhook", time.Now().Add(time.Hour))
	if len(batch) != 1 || batch[0].ID != n.ID {
		t.Fatal("unstable event id")
	}
	if !deliverFleetNotice(context.Background(), batch[0]) {
		t.Fatal("recovery delivery failed")
	}
	if e := re.noticeResult(n.ID, true); e != nil {
		t.Fatal(e)
	}
	final := testStore(t, dir, 10)
	if !final.state.Notices[n.ID].Delivered {
		t.Fatal("ack lost")
	}
	if len(final.noticeBatch("telegram", time.Now().Add(time.Hour))) != 1 {
		t.Fatal("one channel suppressed another")
	}
}
func TestDurableDiskFailurePoisonsAdmission(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, dir, 10)
	path := filepath.Join(s.dir, "state.json")
	os.Remove(path)
	os.Mkdir(path, 0700)
	if _, _, e := s.enqueue(durableJob("p"), "one"); e == nil {
		t.Fatal("write failure ignored")
	}
	if task, e := s.take(map[string]bool{}); e == nil || task != nil {
		t.Fatal("work dispatched from uncommitted memory")
	}
	if _, ok := s.health(); ok {
		t.Fatal("unhealthy store reported healthy")
	}
}
func TestDurableHTTPQueuesBusyAndReportsStatus(t *testing.T) {
	dir := t.TempDir()
	f := triggerFixture(t, dir)
	f.store = testStore(t, dir, 1)
	f.active["parser"] = true
	f.cfg.Jobs[0].Timeout = "1s"
	w := triggerRequest(f, "first")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "pending" {
		t.Fatal(body)
	}
	if w = triggerRequest(f, "second"); w.Code != 429 {
		t.Fatal("queue overflow", w.Code)
	}
	if w = triggerRequest(f, "first"); w.Code != 200 {
		t.Fatal("replay at capacity", w.Code)
	}
	r := httptest.NewRequest("GET", "/tasks/"+body["task_id"].(string), nil)
	r.Header.Set("Authorization", "Bearer test-secret")
	w = httptest.NewRecorder()
	f.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestDurableDispatchExecutesPending(t *testing.T) {
	dir := t.TempDir()
	f := triggerFixture(t, dir)
	f.store = testStore(t, dir, 10)
	task, _, e := f.store.enqueue(durableJob("parser"), "one")
	if e != nil {
		t.Fatal(e)
	}
	f.dispatch()
	f.wg.Wait()
	re := testStore(t, dir, 10)
	if re.state.Tasks[task.ID].Status != "success" {
		t.Fatal(re.state.Tasks[task.ID])
	}
	if _, e := os.Stat(filepath.Join(dir, "parser", task.ID, "result.json")); e != nil {
		t.Fatal(e)
	}
}
func TestFleetCrashHelper(t *testing.T) {
	if os.Getenv("FLEET_CRASH_HELPER") != "1" {
		return
	}
	dir := os.Getenv("FLEET_CRASH_DIR")
	s, e := openFleetStore(dir, 10, []string{"webhook"})
	if e != nil {
		os.Exit(2)
	}
	s.enqueue(durableJob("p"), "running-key")
	s.take(map[string]bool{})
	s.enqueue(durableJob("q"), "pending-key")
	s.notify("test", "fleet", "pending alert")
	os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0600)
	for {
		time.Sleep(time.Hour)
	}
}
func TestDurableRealSIGKILLRecovery(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFleetCrashHelper$")
	cmd.Env = append(os.Environ(), "FLEET_CRASH_HELPER=1", "FLEET_CRASH_DIR="+dir)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case <-deadline.C:
			t.Fatal("helper did not initialize")
		case <-tick.C:
			if _, e := os.Stat(filepath.Join(dir, "ready")); e == nil {
				break wait
			}
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	s := testStore(t, dir, 10)
	running, replay, e := s.enqueue(durableJob("p"), "running-key")
	if e != nil || !replay || running.Status != "interrupted" {
		t.Fatal(running, replay, e)
	}
	pending, replay, e := s.enqueue(durableJob("q"), "pending-key")
	if e != nil || !replay || pending.Status != "pending" {
		t.Fatal(pending, replay, e)
	}
	batch := s.noticeBatch("webhook", time.Now())
	if len(batch) != 2 {
		t.Fatalf("lost crash alerts: %d", len(batch))
	}
}

func TestDurableWorkerConcurrencyAndRestartPending(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-wedra")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.1\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	f := triggerFixture(t, dir)
	f.exe = script
	f.slots = make(chan struct{}, 2)
	f.store = testStore(t, dir, 10)
	for _, entry := range []struct{ job, key string }{{"parser", "one"}, {"parser", "two"}, {"other", "three"}} {
		if _, _, e := f.store.enqueue(durableJob(entry.job), entry.key); e != nil {
			t.Fatal(e)
		}
	}
	f.dispatch()
	f.mu.Lock()
	active := len(f.active)
	f.mu.Unlock()
	if active != 2 || len(f.slots) != 2 {
		t.Fatalf("active=%d slots=%d", active, len(f.slots))
	}
	f.wg.Wait()
	if _, ok := f.store.health(); !ok {
		t.Fatal("store fault")
	}
	// One same-job task stayed pending while first job ran; recreate fleet.
	re := triggerFixture(t, dir)
	re.exe = script
	re.store = testStore(t, dir, 10)
	h, _ := re.store.health()
	if h["pending"] != 1 {
		t.Fatal(h)
	}
	re.dispatch()
	re.wg.Wait()
	h, _ = re.store.health()
	if h["pending"] != 0 || h["running"] != 0 {
		t.Fatal(h)
	}
	for _, task := range re.store.state.Tasks {
		if task.Status != "success" {
			t.Fatal(task.Status)
		}
	}
}

func TestDurableLegacyClaimsNeverRelaunched(t *testing.T) {
	dir := t.TempDir()
	legacy := triggerFixture(t, dir)
	if w := triggerRequest(legacy, "legacy-key"); w.Code != 202 {
		t.Fatal(w.Code)
	}
	legacy.wg.Wait()
	upgraded := triggerFixture(t, dir)
	upgraded.store = testStore(t, dir, 10)
	if w := triggerRequest(upgraded, "legacy-key"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(upgraded.store.state.Tasks) != 0 {
		t.Fatal("legacy claim rerun during migration")
	}
}

func TestDurableCronCoalescesAndRemembersSlots(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, dir, 1)
	a, _, e := s.enqueue(durableJob("p"), "cron:p:minute-1")
	if e != nil {
		t.Fatal(e)
	}
	b, coalesced, e := s.enqueue(durableJob("p"), "cron:p:minute-2")
	if e != nil || !coalesced || b.ID != a.ID {
		t.Fatal("cron pending not coalesced", e)
	}
	re := testStore(t, dir, 1)
	re.take(map[string]bool{})
	c, replay, e := re.enqueue(durableJob("p"), "cron:p:minute-2")
	if e != nil || !replay || c.ID != a.ID {
		t.Fatal("slot replayed after restart", e)
	}
}

func TestDurableCorruptSnapshotRefusesStartup(t *testing.T) {
	dir := t.TempDir()
	s := testStore(t, dir, 10)
	if e := os.WriteFile(filepath.Join(s.dir, "state.json"), []byte("{truncated"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := openFleetStore(dir, 10, []string{"webhook"}); e == nil {
		t.Fatal("corrupt state silently reset")
	}
}
