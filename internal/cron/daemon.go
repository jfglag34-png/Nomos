package cron

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// ScheduleState — куда сохраняем next_run для GUI
type ScheduleState struct {
	Pipeline string     `json:"pipeline"`
	Cron     string     `json:"cron"`
	NextRun  *time.Time `json:"next_run"`
	Enabled  bool       `json:"enabled"`
	Overlap  string     `json:"overlap"`
}

// NextRunFor — считает next_run с учетом timezone
func NextRunFor(sched *pipeline.Schedule, now time.Time) *time.Time {
	if sched == nil || sched.Cron == "" {
		return nil
	}
	loc := time.Local
	if sched.Timezone != "" && sched.Timezone != "Local" && sched.Timezone != "local" {
		if l, err := time.LoadLocation(sched.Timezone); err == nil {
			loc = l
		}
	}
	nowInLoc := now.In(loc)
	n := Next(sched.Cron, nowInLoc)
	if n == nil {
		return nil
	}
	// Next возвращает в loc, конвертим
	return n
}

// ScanPipelines — найти все pipeline.yaml с schedule в dir
func ScanPipelines(dir string) ([]string, error) {
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && (info.Name() == "*.yaml" || filepath.Ext(path) == ".yaml") {
			// quick check: contains schedule:
			b, _ := os.ReadFile(path)
			if containsSchedule(b) {
				out = append(out, path)
			}
		}
		return nil
	})
	// simpler: glob
	if len(out) == 0 {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
		for _, m := range matches {
			b, _ := os.ReadFile(m)
			if containsSchedule(b) {
				out = append(out, m)
			}
		}
		// also examples/
		matches2, _ := filepath.Glob(filepath.Join("examples", "*.yaml"))
		for _, m := range matches2 {
			b, _ := os.ReadFile(m)
			if containsSchedule(b) {
				out = append(out, m)
			}
		}
	}
	return out, err
}

func containsSchedule(b []byte) bool {
	return len(b) > 0 && contains(b, []byte("schedule:")) && contains(b, []byte("cron:"))
}

func contains(b, sub []byte) bool {
	return len(b) >= len(sub) && indexOf(b, sub) >= 0
}
func indexOf(b, sub []byte) int {
	n := len(b) - len(sub) + 1
	for i := 0; i < n; i++ {
		ok := true
		for j := range sub {
			if b[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// Daemon — минимальный демон: тикает каждую минуту, запускает pipeline run
// В wedra это будет вызываться из internal/cli/daemon.go или internal/api/server.go
type Daemon struct {
	PipelinesDir string
	RunsDir      string
	// runner — функция запуска: файл -> run_id
	Runner func(pipelineFile string, autoApprove bool) (string, error)
}

func (d *Daemon) Tick(ctx context.Context) error {
	// сканируем pipelines
	files, _ := ScanPipelines(d.PipelinesDir)
	now := time.Now()
	for _, f := range files {
		pf, err := pipeline.LoadPipelineFile(f)
		if err != nil {
			continue
		}
		sched := pf.Pipeline.Schedule
		if sched == nil || !sched.IsEnabled() {
			continue
		}
		next := NextRunFor(sched, now.Add(-time.Minute))
		if next == nil {
			continue
		}
		// если next в пределах последней минуты — запускаем
		if next.After(now.Add(-time.Minute)) && next.Before(now.Add(5*time.Second)) {
			autoApprove := sched.HumanGate == "autoApprove" || sched.HumanGate == "auto_approve"
			if d.Runner != nil {
				if _, err := d.Runner(f, autoApprove); err != nil {
					fmt.Fprintf(os.Stderr, "schedule %s: %v\n", f, err)
				}
			}
			// алерт on_waiting — через плагин: если human_gate=wait, ран встанет в waiting
			// GUI покажет бейдж, а алерт плагин отправит Telegram/webhook
			_ = sched.Alerts
		}
	}
	return nil
}

func (d *Daemon) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// первый тик сразу
	_ = d.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = d.Tick(ctx)
		}
	}
}
