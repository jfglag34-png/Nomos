//go:build linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func (f *fleet) notify(event, job, detail string) {
	if f.store == nil {
		fleetAlert(event, job, detail)
		return
	}
	if e := f.store.notify(event, job, detail); e != nil {
		log.Printf("CRITICAL: durable alert persistence failed: %v", e)
	}
}
func (f *fleet) dispatch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil {
		return
	}
	for {
		select {
		case f.slots <- struct{}{}:
		default:
			return
		}
		t, e := f.store.take(f.active)
		if e != nil {
			<-f.slots
			log.Printf("CRITICAL: queue dispatch blocked: %v", e)
			return
		}
		if t == nil {
			<-f.slots
			return
		}
		f.active[t.Job.ID] = true
		f.wg.Add(1)
		go func(task fleetTask) {
			defer f.wg.Done()
			event := f.runTracked(task.Job, task.ID)
			// Durable completion + alert before releasing the job's exclusion slot.
			if e := f.store.finish(task.ID, event); e != nil {
				log.Printf("CRITICAL: task=%s completion persistence failed: %v", task.ID, e)
			}
			f.mu.Lock()
			delete(f.active, task.Job.ID)
			f.mu.Unlock()
			<-f.slots
		}(*t)
	}
}
func fleetChannels() []string {
	out := []string{}
	if os.Getenv("WEDRA_ALERT_WEBHOOK") != "" {
		out = append(out, "webhook")
	}
	if os.Getenv("WEDRA_TELEGRAM_TOKEN") != "" && os.Getenv("WEDRA_TELEGRAM_CHAT_ID") != "" {
		out = append(out, "telegram")
	}
	return out
}
func deliverFleetNotice(ctx context.Context, n fleetNotice) bool {
	var url string
	var payload interface{}
	switch n.Channel {
	case "webhook":
		url = os.Getenv("WEDRA_ALERT_WEBHOOK")
		payload = map[string]interface{}{"event_id": n.ID, "event": n.Event, "job": n.Job, "detail": n.Detail, "at": n.At.Format(time.RFC3339)}
	case "telegram":
		token, chat := os.Getenv("WEDRA_TELEGRAM_TOKEN"), os.Getenv("WEDRA_TELEGRAM_CHAT_ID")
		if token == "" || chat == "" {
			return false
		}
		url = "https://api.telegram.org/bot" + token + "/sendMessage"
		payload = map[string]string{"chat_id": chat, "text": fmt.Sprintf("WEDRA %s\n%s\n%s\nevent_id=%s", n.Event, n.Job, n.Detail, n.ID)}
	default:
		return false
	}
	if url == "" {
		return false
	}
	raw, e := json.Marshal(payload)
	if e != nil {
		return false
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if e != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", n.ID)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if e != nil {
		return false
	}
	if n.Channel == "telegram" {
		var result struct {
			OK bool `json:"ok"`
		}
		return json.Unmarshal(body, &result) == nil && result.OK
	}
	return true
}
func (f *fleet) deliveryLoop(ctx context.Context, channel string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for _, n := range f.store.noticeBatch(channel, time.Now()) {
			if ctx.Err() != nil {
				return
			}
			success := deliverFleetNotice(ctx, n)
			if ctx.Err() != nil {
				return
			} // Leave ambiguous delivery pending for restart.
			if e := f.store.noticeResult(n.ID, success); e != nil {
				log.Printf("CRITICAL: alert state persistence failed: %v", e)
				return
			}
			if !success {
				log.Printf("alert pending channel=%s event_id=%s attempts=%d", channel, n.ID, n.Attempts+1)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
