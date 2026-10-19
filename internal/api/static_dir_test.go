package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Регресс: фронтенд не должен зависеть от текущего каталога.
//
// Раньше Routes() делал `os.Stat("web/static")` и, если каталог был виден из
// CWD, отдавал GUI с диска. Это делало отдаваемое содержимое свойством места
// запуска: подложенный ./web/static/index.html подменял GUI, а после входа
// этот JS жил в origin с полной сессией (журналы шагов + запуск ранов с --yes).
// Правильное поведение: без явного StaticDir фронтенд всегда встроенный.
func TestStaticIsNotPickedUpFromWorkingDir(t *testing.T) {
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	pipelines := filepath.Join(dir, "pipelines")
	runs := filepath.Join(dir, "runs")
	for _, d := range []string{plugins, pipelines, runs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Подложенный фронтенд в текущем каталоге — ровно то, что раньше
	// подхватывалось молча.
	attacker := filepath.Join(dir, "web", "static")
	if err := os.MkdirAll(attacker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attacker, "index.html"),
		[]byte("<html><body>ATTACKER PAGE</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	srv := NewServer(plugins, pipelines, runs)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(body), "ATTACKER PAGE") {
		t.Fatalf("GUI отдан с диска из текущего каталога, а не из бинарника:\n%s", body)
	}
	if !strings.Contains(string(body), "Nomos") {
		t.Fatalf("встроенный GUI не отдан (нет маркера Nomos):\n%s", body)
	}
}

// Явно названный каталог — это dev-режим, и он обязан работать: правки JS
// без пересборки бинарника.
func TestExplicitStaticDirIsServed(t *testing.T) {
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	pipelines := filepath.Join(dir, "pipelines")
	runs := filepath.Join(dir, "runs")
	for _, d := range []string{plugins, pipelines, runs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	static := filepath.Join(dir, "front")
	if err := os.MkdirAll(static, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(static, "index.html"),
		[]byte("<html><body>DEV FRONTEND</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(plugins, pipelines, runs)
	srv.StaticDir = static
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "DEV FRONTEND") {
		t.Fatalf("--static=<dir> не отдаёт названный каталог:\n%s", body)
	}
}
