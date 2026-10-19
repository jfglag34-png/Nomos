//go:build linux

package cli

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wykserdex/wedra/internal/cron"
	"gopkg.in/yaml.v3"
)

type fleetJob struct {
	Every       string `yaml:"every"`
	interval    time.Duration
	ID          string `yaml:"id"`
	File        string `yaml:"file"`
	Cron        string `yaml:"cron"`
	Timezone    string `yaml:"timezone"`
	Timeout     string `yaml:"timeout"`
	AutoApprove bool   `yaml:"auto_approve"`
	duration    time.Duration
	location    *time.Location
}
type fleetConfig struct {
	Concurrency int        `yaml:"concurrency"`
	Listen      string     `yaml:"listen"`
	RunsDir     string     `yaml:"runs_dir"`
	Jobs        []fleetJob `yaml:"jobs"`
}
type fleet struct {
	idemMu sync.Mutex
	cfg    fleetConfig
	mu     sync.Mutex
	active map[string]bool
	slots  chan struct{}
	wg     sync.WaitGroup
	ctx    context.Context
	exe    string
}

func loadFleet(path string) (fleetConfig, error) {
	var c fleetConfig
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if c.Concurrency == 0 {
		c.Concurrency = 6
	}
	if c.Concurrency < 1 || c.Concurrency > 64 {
		return c, fmt.Errorf("concurrency must be 1..64")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8770"
	}
	if c.RunsDir == "" {
		c.RunsDir = "var/fleet"
	}
	if len(c.Jobs) == 0 {
		return c, fmt.Errorf("jobs is empty")
	}
	seen := map[string]bool{}
	for i := range c.Jobs {
		j := &c.Jobs[i]
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(j.ID) || seen[j.ID] {
			return c, fmt.Errorf("invalid/duplicate job id %q", j.ID)
		}
		seen[j.ID] = true
		if j.File == "" {
			return c, fmt.Errorf("%s: file required", j.ID)
		}
		if _, e = os.Stat(j.File); e != nil {
			return c, e
		}
		if j.Timeout == "" {
			j.Timeout = "30m"
		}
		j.duration, e = time.ParseDuration(j.Timeout)
		if e != nil || j.duration <= 0 {
			return c, fmt.Errorf("%s: invalid timeout", j.ID)
		}
		if j.Timezone == "" {
			j.Timezone = "UTC"
		}
		j.location, e = time.LoadLocation(j.Timezone)
		if e != nil {
			return c, e
		}
		if j.Every != "" {
			if j.Cron != "" {
				return c, fmt.Errorf("%s: use cron OR every", j.ID)
			}
			j.interval, e = time.ParseDuration(j.Every)
			if e != nil || j.interval < time.Minute {
				return c, fmt.Errorf("%s: every must be at least 1m", j.ID)
			}
		}
		if j.Cron != "" && !cron.IsValid(j.Cron) {
			return c, fmt.Errorf("%s: invalid cron", j.ID)
		}
	}
	return c, nil
}

// Admission is atomic: no duplicate execution and no unbounded trigger queue.
func (f *fleet) start(j fleetJob) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil || f.active[j.ID] {
		return false
	}
	select {
	case f.slots <- struct{}{}:
	default:
		return false
	}
	f.active[j.ID] = true
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer func() { f.mu.Lock(); delete(f.active, j.ID); f.mu.Unlock(); <-f.slots }()
		f.run(j)
	}()
	return true
}
func (f *fleet) run(j fleetJob) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	dir := filepath.Join(f.cfg.RunsDir, j.ID, stamp)
	if e := os.MkdirAll(dir, 0700); e != nil {
		fleetAlert("failure", j.ID, "cannot create run directory")
		return
	}
	out, e := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		fleetAlert("failure", j.ID, "cannot open run log")
		return
	}
	defer out.Close()
	args := []string{"pipeline", "run", j.File, "--runs-dir=" + filepath.Join(dir, "runs")}
	if j.AutoApprove {
		args = append(args, "--yes")
	}
	cmd := exec.Command(f.exe, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	event, detail := "success", "completed"
	if e = cmd.Start(); e != nil {
		event, detail = "failure", "could not start pipeline"
	} else {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		timer := time.NewTimer(j.duration)
		defer timer.Stop()
		select {
		case e = <-done:
			if e != nil {
				event, detail = "failure", e.Error()
			}
		case <-timer.C:
			event, detail = "timeout", "runtime exceeded "+j.Timeout
			// Notify before cleanup; a hung pipeline must not delay the alert.
			go fleetAlert(event, j.ID, detail)
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		case <-f.ctx.Done():
			event, detail = "cancelled", "fleet shutting down"
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		// Reap descendants even when their direct parent has exited.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	b, _ := json.Marshal(map[string]string{"event": event, "job": j.ID, "detail": detail, "at": time.Now().UTC().Format(time.RFC3339)})
	if e = os.WriteFile(filepath.Join(dir, "result.json"), b, 0600); e != nil {
		log.Printf("result write: %v", e)
	}
	log.Printf("job=%s event=%s log=%s", j.ID, event, dir)
	if event != "timeout" {
		fleetAlert(event, j.ID, detail)
	}
}

// Channels are independent; a broken webhook cannot suppress Telegram.
func fleetAlert(event, id, detail string) {
	var wg sync.WaitGroup
	send := func(label, url string, payload any) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := json.Marshal(payload)
			client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			for attempt := 0; attempt < 3; attempt++ {
				req, e := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
				if e != nil {
					log.Printf("alert %s: invalid URL", label)
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, e := client.Do(req)
				if e == nil {
					io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
					resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						return
					}
				}
				// Never log URLs: Telegram embeds the bot token in its URL.
				log.Printf("alert delivery failed channel=%s attempt=%d event=%s job=%s", label, attempt+1, event, id)
				if attempt < 2 {
					time.Sleep(time.Duration(attempt+1) * time.Second)
				}
			}
		}()
	}
	if url := os.Getenv("WEDRA_ALERT_WEBHOOK"); url != "" {
		send("webhook", url, map[string]string{"event": event, "job": id, "detail": detail, "at": time.Now().UTC().Format(time.RFC3339)})
	}
	token, chat := os.Getenv("WEDRA_TELEGRAM_TOKEN"), os.Getenv("WEDRA_TELEGRAM_CHAT_ID")
	if token != "" && chat != "" {
		send("telegram", "https://api.telegram.org/bot"+token+"/sendMessage", map[string]string{"chat_id": chat, "text": fmt.Sprintf("WEDRA %s\n%s\n%s", event, id, detail)})
	}
	wg.Wait()
}
func (f *fleet) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("WEDRA_TRIGGER_TOKEN")
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/trigger/")
		if r.URL.Path != "/trigger/"+id {
			http.NotFound(w, r)
			return
		}
		for _, j := range f.cfg.Jobs {
			if j.ID == id {
				f.trigger(w, r, j)
				return
			}
		}
		http.NotFound(w, r)
	})
}
func RunFleet(args []string) {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	path := fs.String("config", "fleet.yaml", "fleet YAML (paths relative to working directory)")
	check := fs.Bool("check", false, "validate config only")
	if e := fs.Parse(args); e != nil {
		return
	}
	c, e := loadFleet(*path)
	if e != nil {
		log.Printf("fleet config: %v", e)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("OK: %d jobs, concurrency %d\n", len(c.Jobs), c.Concurrency)
		return
	}
	if os.Getenv("WEDRA_TRIGGER_TOKEN") == "" {
		log.Fatal("WEDRA_TRIGGER_TOKEN required")
	}
	if os.Getenv("WEDRA_ALERT_WEBHOOK") == "" && (os.Getenv("WEDRA_TELEGRAM_TOKEN") == "" || os.Getenv("WEDRA_TELEGRAM_CHAT_ID") == "") {
		log.Fatal("configure Telegram or WEDRA_ALERT_WEBHOOK")
	}
	if e = os.MkdirAll(c.RunsDir, 0700); e != nil {
		log.Fatal(e)
	}
	lock, e := os.OpenFile(filepath.Join(c.RunsDir, "fleet.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		log.Fatal(e)
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		log.Fatal("fleet already running for this runs_dir")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exe, e := os.Executable()
	if e != nil {
		log.Fatal(e)
	}
	f := &fleet{cfg: c, ctx: ctx, exe: exe, active: map[string]bool{}, slots: make(chan struct{}, c.Concurrency)}
	listener, e := net.Listen("tcp", c.Listen)
	if e != nil {
		log.Fatal(e)
	}
	server := &http.Server{Handler: f.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		if e := server.Serve(listener); e != nil && !errors.Is(e, http.ErrServerClosed) {
			log.Printf("fleet HTTP stopped: %v", e)
			cancel()
		}
	}()
	fleetAlert("started", "fleet", "scheduler started")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := time.Time{}
	nextInterval := map[string]time.Time{}
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-ticker.C:
			for _, j := range c.Jobs {
				if j.interval > 0 && !now.Before(nextInterval[j.ID]) {
					if !f.start(j) {
						log.Printf("job=%s skipped: busy/capacity", j.ID)
					}
					nextInterval[j.ID] = now.Add(j.interval)
				}
			}
			minute := now.Truncate(time.Minute)
			if minute.Equal(last) {
				continue
			}
			last = minute
			for _, j := range c.Jobs {
				if j.Cron == "" {
					continue
				}
				next := cron.Next(j.Cron, minute.In(j.location).Add(-time.Minute))
				if next != nil && next.Equal(minute) {
					if !f.start(j) {
						log.Printf("job=%s skipped: busy/capacity", j.ID)
					}
				}
			}
		}
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	server.Shutdown(shutdown)
	f.mu.Lock()
	f.mu.Unlock()
	f.wg.Wait()
	fleetAlert("stopped", "fleet", "scheduler stopped")
}
