package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

func TestNextRunFor(t *testing.T) {
	if NextRunFor(nil, time.Now()) != nil {
		t.Error("nil sched")
	}
	s := &pipeline.Schedule{Cron: ""}
	if NextRunFor(s, time.Now()) != nil {
		t.Error("empty cron")
	}
	s = &pipeline.Schedule{Cron: "0 9 * * *", Timezone: "UTC"}
	now := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	n := NextRunFor(s, now)
	if n == nil || n.Hour() != 9 {
		t.Fatalf("next 09 UTC got %v", n)
	}
	s.Timezone = "Local"
	n = NextRunFor(s, now)
	if n == nil {
		t.Fatal("Local tz nil")
	}
	s.Timezone = "local"
	n = NextRunFor(s, now)
	if n == nil {
		t.Fatal("local tz nil")
	}
	s.Timezone = "Europe/Minsk"
	n = NextRunFor(s, now)
	if n == nil {
		t.Fatal("Europe/Minsk nil")
	}
	// bad cron
	s.Cron = "bad"
	if NextRunFor(s, now) != nil {
		t.Error("bad cron should nil")
	}
}

func TestContainsSchedule(t *testing.T) {
	if !containsSchedule([]byte("pipeline:\n schedule:\n  cron: \"* * * * *\"")) {
		t.Error("should contain")
	}
	if containsSchedule([]byte("nope")) {
		t.Error("should not")
	}
	if containsSchedule([]byte("")) {
		t.Error("empty should not")
	}
	if !contains([]byte("hello"), []byte("ell")) {
		t.Error("contains")
	}
	if contains([]byte("hi"), []byte("hello")) {
		t.Error("not contains")
	}
	if indexOf([]byte("abc"), []byte("bc")) != 1 {
		t.Error("indexOf")
	}
	if indexOf([]byte("abc"), []byte("d")) != -1 {
		t.Error("indexOf -1")
	}
	if indexOf([]byte("a"), []byte("aa")) != -1 {
		t.Error("long sub")
	}
}

func TestScanPipelines(t *testing.T) {
	dir := t.TempDir()
	// create files
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("pipeline:\n schedule:\n  cron: \"* * * * *\""), 0o644)
	os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("pipeline:\n name: x"), 0o644)
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "c.yaml"), []byte("pipeline:\n schedule:\n  cron: \"0 * * * *\""), 0o644)
	// also create file with .yaml but without schedule
	os.WriteFile(filepath.Join(dir, "d.yaml"), []byte("not schedule"), 0o644)

	out, err := ScanPipelines(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 2 {
		t.Fatalf("expected >=2 got %v", out)
	}
	// check that a.yaml and sub/c.yaml are present
	foundA := false
	foundC := false
	for _, p := range out {
		if p == filepath.Join(dir, "a.yaml") {
			foundA = true
		}
		if p == filepath.Join(sub, "c.yaml") {
			foundC = true
		}
	}
	if !foundA || !foundC {
		t.Fatalf("missing expected files in %v", out)
	}
	// empty dir fallback to glob
	empty := t.TempDir()
	out2, _ := ScanPipelines(empty)
	_ = out2 // should be empty but not crash
}

func TestDaemonTick(t *testing.T) {
	dir := t.TempDir()
	// create pipeline with schedule that would trigger now
	// We need file that NextRunFor(now-1m) is within last minute
	// Use every minute
	now := time.Now()
	cron := "* * * * *"
	// write file with schedule every minute, autoApprove
	content := "format_version: \"0.2\"\npipeline:\n  name: ticktest\n  input: {}\n  steps: []\n  schedule:\n    cron: \"" + cron + "\"\n    human_gate: autoApprove\n"
	p := filepath.Join(dir, "p.yaml")
	os.WriteFile(p, []byte(content), 0o644)

	calls := 0
	d := &Daemon{
		PipelinesDir: dir,
		RunsDir:      t.TempDir(),
		Runner: func(pf string, auto bool) (string, error) {
			calls++
			if !auto {
				t.Error("expected autoApprove")
			}
			return "run1", nil
		},
	}
	// Tick should potentially call runner (timing dependent, but we at least check it doesn't crash)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Also test with no Runner (nil) should not panic
	d.Runner = nil
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// test with disabled schedule
	os.WriteFile(p, []byte("format_version: \"0.2\"\npipeline:\n  name: x\n  input: {}\n  steps: []\n"), 0o644)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = now
}

func TestDaemonRunCancel(t *testing.T) {
	d := &Daemon{PipelinesDir: t.TempDir(), Runner: func(string, bool) (string, error) { return "", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx); err == nil {
		t.Fatal("expected cancel error")
	}
}
