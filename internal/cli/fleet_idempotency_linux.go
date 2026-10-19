//go:build linux

package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type triggerReceipt struct {
	ID        string `json:"trigger_id"`
	Job       string `json:"job"`
	ClaimedAt string `json:"claimed_at"`
}

func syncDirectory(path string) error {
	d, e := os.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

// Persist before launch. A durable claim is deliberately never replayed as work:
// after a crash we cannot distinguish "not started" from "external effects happened".
func (f *fleet) trigger(w http.ResponseWriter, r *http.Request, j fleetJob) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		http.Error(w, "Idempotency-Key required (1..128 printable ASCII characters, no surrounding whitespace)", http.StatusBadRequest)
		return
	}
	for _, c := range []byte(key) {
		if c < 33 || c > 126 {
			http.Error(w, "invalid Idempotency-Key", http.StatusBadRequest)
			return
		}
	}
	if f.store != nil {
		if f.ctx.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		hash := sha256.Sum256([]byte(j.ID + "\x00" + key))
		// Do not reinterpret legacy claims as fresh submissions after upgrade.
		legacy := filepath.Join(f.cfg.RunsDir, ".trigger-claims", hex.EncodeToString(hash[:])+".json")
		if raw, e := os.ReadFile(legacy); e == nil {
			var receipt triggerReceipt
			if json.Unmarshal(raw, &receipt) != nil || receipt.ID == "" || receipt.Job != j.ID {
				http.Error(w, "legacy claim corrupt", http.StatusServiceUnavailable)
				return
			}
			writeTriggerResponse(w, 200, receipt, true)
			return
		} else if !errors.Is(e, os.ErrNotExist) {
			http.Error(w, "legacy claim store unavailable", http.StatusServiceUnavailable)
			return
		}
		task, replay, e := f.store.enqueue(j, hex.EncodeToString(hash[:]))
		if e != nil {
			code := 503
			if errors.Is(e, errFleetQueueFull) {
				code = 429
				w.Header().Set("Retry-After", "5")
			}
			http.Error(w, "queue unavailable; retry SAME key", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if replay {
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(200)
		} else {
			w.WriteHeader(202)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"trigger_id": task.ID, "task_id": task.ID, "job": task.Job.ID, "replayed": replay, "status": task.Status, "claimed_at": task.Created})
		return
	}
	f.idemMu.Lock()
	defer f.idemMu.Unlock()
	dir := filepath.Join(f.cfg.RunsDir, ".trigger-claims")
	// The runs directory is operator-controlled, not writable by webhook callers.
	if e := os.MkdirAll(dir, 0700); e != nil {
		http.Error(w, "claim store unavailable", http.StatusServiceUnavailable)
		return
	}
	if e := syncDirectory(f.cfg.RunsDir); e != nil {
		http.Error(w, "claim store unavailable", http.StatusServiceUnavailable)
		return
	}
	hash := sha256.Sum256([]byte(j.ID + "\x00" + key))
	path := filepath.Join(dir, hex.EncodeToString(hash[:])+".json")
	existing, e := os.ReadFile(path)
	if e == nil {
		var receipt triggerReceipt
		if json.Unmarshal(existing, &receipt) != nil || receipt.ID == "" || receipt.Job != j.ID {
			http.Error(w, "claim store corrupt; operator inspection required", http.StatusServiceUnavailable)
			return
		}
		writeTriggerResponse(w, 200, receipt, true)
		return
	}
	if !errors.Is(e, os.ErrNotExist) {
		http.Error(w, "claim store unavailable", http.StatusServiceUnavailable)
		return
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		http.Error(w, "cannot create trigger id", http.StatusServiceUnavailable)
		return
	}
	receipt := triggerReceipt{ID: hex.EncodeToString(random[:]), Job: j.ID, ClaimedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(receipt)
	// Exclusive creation + fsync before process admission. Incomplete records fail
	// closed, never becoming permission to execute a duplicate after restart.
	file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		http.Error(w, "claim persistence failed", http.StatusServiceUnavailable)
		return
	}
	_, e = file.Write(raw)
	if e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = syncDirectory(dir)
	}
	if e != nil {
		http.Error(w, "claim persistence uncertain; inspect before retrying with a new key", http.StatusServiceUnavailable)
		return
	}
	if !f.start(j) {
		// No process admitted: caller may safely retry the SAME key after 409.
		if e = os.Remove(path); e == nil {
			e = syncDirectory(dir)
		}
		if e != nil {
			http.Error(w, "claim cleanup uncertain; operator inspection required", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "job running, shutting down or capacity reached; retry same key", http.StatusConflict)
		return
	}
	writeTriggerResponse(w, 202, receipt, false)
}
func writeTriggerResponse(w http.ResponseWriter, status int, receipt triggerReceipt, replay bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if replay {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"trigger_id": receipt.ID, "job": receipt.Job, "claimed_at": receipt.ClaimedAt,
		"replayed": replay, "status": map[bool]string{true: "claimed", false: "accepted"}[replay],
		"execution_status": "not_reported", "detail": fmt.Sprintf("Claim retained; this response does not confirm pipeline completion. replay=%t", replay),
	})
}
