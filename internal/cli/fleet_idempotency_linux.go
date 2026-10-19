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
		http.Error(w, "Idempotency-Key required (1..128 printable ASCII characters, no surrounding whitespace)", 400)
		return
	}
	for _, c := range []byte(key) {
		if c < 33 || c > 126 {
			http.Error(w, "invalid Idempotency-Key", 400)
			return
		}
	}
	f.idemMu.Lock()
	defer f.idemMu.Unlock()
	dir := filepath.Join(f.cfg.RunsDir, ".trigger-claims")
	// The runs directory is operator-controlled, not writable by webhook callers.
	if e := os.MkdirAll(dir, 0700); e != nil {
		http.Error(w, "claim store unavailable", 503)
		return
	}
	if e := syncDirectory(f.cfg.RunsDir); e != nil {
		http.Error(w, "claim store unavailable", 503)
		return
	}
	hash := sha256.Sum256([]byte(j.ID + "\x00" + key))
	path := filepath.Join(dir, hex.EncodeToString(hash[:])+".json")
	existing, e := os.ReadFile(path)
	if e == nil {
		var receipt triggerReceipt
		if json.Unmarshal(existing, &receipt) != nil || receipt.ID == "" || receipt.Job != j.ID {
			http.Error(w, "claim store corrupt; operator inspection required", 503)
			return
		}
		writeTriggerResponse(w, 200, receipt, true)
		return
	}
	if !errors.Is(e, os.ErrNotExist) {
		http.Error(w, "claim store unavailable", 503)
		return
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		http.Error(w, "cannot create trigger id", 503)
		return
	}
	receipt := triggerReceipt{ID: hex.EncodeToString(random[:]), Job: j.ID, ClaimedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(receipt)
	// Exclusive creation + fsync before process admission. Incomplete records fail
	// closed, never becoming permission to execute a duplicate after restart.
	file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		http.Error(w, "claim persistence failed", 503)
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
		http.Error(w, "claim persistence uncertain; inspect before retrying with a new key", 503)
		return
	}
	if !f.start(j) {
		// No process admitted: caller may safely retry the SAME key after 409.
		if e = os.Remove(path); e == nil {
			e = syncDirectory(dir)
		}
		if e != nil {
			http.Error(w, "claim cleanup uncertain; operator inspection required", 503)
			return
		}
		http.Error(w, "job running, shutting down or capacity reached; retry same key", 409)
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
