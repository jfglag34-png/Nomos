//go:build linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type fleetTask struct {
	ID      string    `json:"id"`
	Job     fleetJob  `json:"job"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}
type fleetNotice struct {
	ID        string    `json:"id"`
	Event     string    `json:"event"`
	Job       string    `json:"job"`
	Detail    string    `json:"detail"`
	Channel   string    `json:"channel"`
	At        time.Time `json:"at"`
	Attempts  int       `json:"attempts"`
	Next      time.Time `json:"next"`
	Delivered bool      `json:"delivered"`
}
type fleetDisk struct {
	NextInterval map[string]time.Time    `json:"next_interval"`
	Version      int                     `json:"version"`
	Tasks        map[string]*fleetTask   `json:"tasks"`
	Claims       map[string]string       `json:"claims"`
	Notices      map[string]*fleetNotice `json:"notices"`
}
type fleetStore struct {
	mu       sync.Mutex
	dir      string
	state    fleetDisk
	fault    error
	channels []string
	limit    int
}

func fleetID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func openFleetStore(runs string, limit int, channels []string) (*fleetStore, error) {
	if limit < 1 {
		limit = 1000
	}
	dir := filepath.Join(runs, ".durable")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	if e := syncDirectory(runs); e != nil {
		return nil, e
	}
	s := &fleetStore{dir: dir, limit: limit, channels: channels, state: fleetDisk{Version: 1, Tasks: map[string]*fleetTask{}, Claims: map[string]string{}, Notices: map[string]*fleetNotice{}}}
	raw, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e == nil {
		if e = json.Unmarshal(raw, &s.state); e != nil {
			return nil, fmt.Errorf("durable state corrupt: %w", e)
		}
		if s.state.Version != 1 || s.state.Tasks == nil || s.state.Claims == nil || s.state.Notices == nil {
			return nil, fmt.Errorf("invalid durable state")
		}
		for id, t := range s.state.Tasks {
			if t == nil || t.ID != id || t.Job.ID == "" {
				return nil, fmt.Errorf("invalid task record")
			}
			switch t.Status {
			case "pending", "running", "success", "failure", "timeout", "cancelled", "interrupted":
			default:
				return nil, fmt.Errorf("invalid task status")
			}
			if t.Status == "pending" || t.Status == "running" {
				d, e := time.ParseDuration(t.Job.Timeout)
				if e != nil || d <= 0 {
					return nil, fmt.Errorf("invalid queued timeout")
				}
				t.Job.duration = d
			}
		}
		for _, id := range s.state.Claims {
			if s.state.Tasks[id] == nil {
				return nil, fmt.Errorf("dangling idempotency claim")
			}
		}
		for id, n := range s.state.Notices {
			if n == nil || n.ID != id || (n.Channel != "telegram" && n.Channel != "webhook") {
				return nil, fmt.Errorf("invalid notice record")
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if s.state.NextInterval == nil {
		s.state.NextInterval = map[string]time.Time{}
	}
	// Persist interruption + its alert in the same snapshot; never replay uncertain work.
	for _, t := range s.state.Tasks {
		if t.Status == "running" {
			t.Status = "interrupted"
			t.Updated = time.Now().UTC()
			s.addNotices("interrupted", t.Job.ID, "task "+t.ID+": daemon stopped during execution; manual inspection required")
		}
	}
	if e = s.commit(); e != nil {
		return nil, e
	}
	return s, nil
}

// Atomic snapshot transaction on a local filesystem. Any uncertain write poisons
// admission and dispatch until restart: never proceed from only in-memory state.
func (s *fleetStore) commit() error {
	if s.fault != nil {
		return s.fault
	}
	raw, e := json.Marshal(s.state)
	if e != nil {
		s.fault = e
		return e
	}
	file, e := os.CreateTemp(s.dir, ".state-*")
	if e != nil {
		s.fault = e
		return e
	}
	name := file.Name()
	defer os.Remove(name)
	if _, e = file.Write(raw); e == nil {
		e = file.Sync()
	}
	ce := file.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, filepath.Join(s.dir, "state.json"))
	}
	if e == nil {
		e = syncDirectory(s.dir)
	}
	if e != nil {
		s.fault = e
	}
	return e
}
func (s *fleetStore) addNotices(event, job, detail string) {
	for _, channel := range s.channels {
		id := fleetID()
		s.state.Notices[id] = &fleetNotice{ID: id, Event: event, Job: job, Detail: detail, Channel: channel, At: time.Now().UTC()}
	}
}
func (s *fleetStore) notify(event, job, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return s.fault
	}
	s.addNotices(event, job, detail)
	return s.commit()
}

var errFleetQueueFull = errors.New("durable queue full")

// key is already scoped and hashed; empty key is a scheduled submission.
func (s *fleetStore) enqueue(j fleetJob, key string) (fleetTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return fleetTask{}, false, s.fault
	}
	if key != "" {
		if id := s.state.Claims[key]; id != "" {
			return *s.state.Tasks[id], true, nil
		}
	}
	count := 0
	for _, t := range s.state.Tasks {
		if t.Status == "pending" {
			count++
			if (key == "" || strings.HasPrefix(key, "cron:")) && t.Job.ID == j.ID {
				if key != "" {
					s.state.Claims[key] = t.ID
					if e := s.commit(); e != nil {
						return fleetTask{}, false, e
					}
				}
				return *t, true, nil
			}
		}
	}
	if count >= s.limit {
		return fleetTask{}, false, errFleetQueueFull
	}
	now := time.Now().UTC()
	id := fleetID()
	task := &fleetTask{ID: id, Job: j, Status: "pending", Created: now, Updated: now}
	s.state.Tasks[id] = task
	if key != "" {
		s.state.Claims[key] = id
	}
	if e := s.commit(); e != nil {
		return fleetTask{}, false, e
	}
	return *task, false, nil
}

// Caller serializes dispatch with completion using fleet.mu.
func (s *fleetStore) take(active map[string]bool) (*fleetTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return nil, s.fault
	}
	list := []*fleetTask{}
	for _, t := range s.state.Tasks {
		if t.Status == "pending" && !active[t.Job.ID] {
			list = append(list, t)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Created.Equal(list[j].Created) {
			return list[i].ID < list[j].ID
		}
		return list[i].Created.Before(list[j].Created)
	})
	if len(list) == 0 {
		return nil, nil
	}
	t := list[0]
	t.Status = "running"
	t.Updated = time.Now().UTC()
	if e := s.commit(); e != nil {
		return nil, e
	}
	copy := *t
	return &copy, nil
}
func (s *fleetStore) finish(id, event string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return s.fault
	}
	t := s.state.Tasks[id]
	if t == nil {
		return fmt.Errorf("unknown task")
	}
	t.Status = event
	t.Updated = time.Now().UTC()
	if event != "timeout" {
		s.addNotices(event, t.Job.ID, "task "+id+": "+event)
	}
	return s.commit()
}
func (s *fleetStore) noticeBatch(channel string, now time.Time) []fleetNotice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return nil
	}
	list := []fleetNotice{}
	for _, n := range s.state.Notices {
		if !n.Delivered && n.Channel == channel && !n.Next.After(now) {
			list = append(list, *n)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	if len(list) > 16 {
		list = list[:16]
	}
	return list
}
func (s *fleetStore) noticeResult(id string, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return s.fault
	}
	n := s.state.Notices[id]
	if n == nil {
		return fmt.Errorf("unknown notice")
	}
	n.Attempts++
	n.Delivered = success
	delay := time.Second * 5
	for i := 1; i < n.Attempts && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	n.Next = time.Now().UTC().Add(delay)
	return s.commit()
}
func (s *fleetStore) health() (map[string]interface{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, running, alerts, interrupted := 0, 0, 0, 0
	for _, t := range s.state.Tasks {
		if t.Status == "pending" {
			pending++
		}
		if t.Status == "running" {
			running++
		}
		if t.Status == "interrupted" {
			interrupted++
		}
	}
	for _, n := range s.state.Notices {
		if !n.Delivered {
			alerts++
		}
	}
	return map[string]interface{}{"ok": s.fault == nil, "pending": pending, "running": running, "undelivered_alerts": alerts, "interrupted": interrupted, "queue_limit": s.limit}, s.fault == nil
}

// Persist interval checkpoints and task admission in one snapshot.
func (s *fleetStore) scheduleInterval(j fleetJob, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return s.fault
	}
	if now.Before(s.state.NextInterval[j.ID]) {
		return nil
	}
	count := 0
	coalesced := false
	for _, t := range s.state.Tasks {
		if t.Status == "pending" {
			count++
			if t.Job.ID == j.ID {
				coalesced = true
			}
		}
	}
	if !coalesced {
		if count >= s.limit {
			return errFleetQueueFull
		}
		id := fleetID()
		s.state.Tasks[id] = &fleetTask{ID: id, Job: j, Status: "pending", Created: now.UTC(), Updated: now.UTC()}
	}
	s.state.NextInterval[j.ID] = now.Add(j.interval).UTC()
	return s.commit()
}
