package api

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandlePresetsMissing(t *testing.T) {
	s := NewServer(t.TempDir(), t.TempDir(), t.TempDir())
	// PluginsDir is temp, root won't have registry.yaml
	req := httptest.NewRequest("GET", "/api/presets", nil)
	w := httptest.NewRecorder()
	s.handlePresets(w, req)
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
}

func TestHandlePresetsAvailable(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "registry.yaml"), []byte("version: \"0.1\"\nplugins: {}\npresets:\n  demo:\n    source: https://example.com\n    path: x.yaml\n    description: test\n"), 0o644)
	pluginsDir := filepath.Join(root, "plugins")
	os.MkdirAll(pluginsDir, 0o755)
	pipelinesDir := filepath.Join(root, "pipelines")
	os.MkdirAll(pipelinesDir, 0o755)
	s := NewServer(pluginsDir, pipelinesDir, t.TempDir())
	req := httptest.NewRequest("GET", "/api/presets", nil)
	w := httptest.NewRecorder()
	s.handlePresets(w, req)
	if w.Code != 200 {
		t.Fatalf("code %d body %s", w.Code, w.Body.String())
	}
}

func TestHandlePipelineDetail(t *testing.T) {
	pDir := t.TempDir()
	s := NewServer(t.TempDir(), pDir, t.TempDir())
	// missing file
	req := httptest.NewRequest("GET", "/api/pipelines/notfound.yaml", nil)
	w := httptest.NewRecorder()
	s.handlePipelineDetail(w, req)
	if w.Code != 404 && w.Code != 400 {
		t.Fatalf("expected 404 got %d", w.Code)
	}
	// create file
	os.WriteFile(filepath.Join(pDir, "a.yaml"), []byte("format_version: \"0.2\"\npipeline:\n  name: test\n  input: {}\n  steps: []\n"), 0o644)
	req = httptest.NewRequest("GET", "/api/pipelines/a.yaml", nil)
	w = httptest.NewRecorder()
	s.handlePipelineDetail(w, req)
	if w.Code != 200 {
		t.Fatalf("get %d", w.Code)
	}
	// PUT valid
	body := "format_version: \"0.2\"\npipeline:\n  name: test2\n  input: {}\n  steps: []\n"
	req = httptest.NewRequest("PUT", "/api/pipelines/a.yaml", bytes.NewReader([]byte(body)))
	w = httptest.NewRecorder()
	s.handlePipelineDetail(w, req)
	if w.Code != 200 {
		t.Fatalf("put %d %s", w.Code, w.Body.String())
	}
	// PUT invalid yaml
	req = httptest.NewRequest("PUT", "/api/pipelines/a.yaml", bytes.NewReader([]byte("bad YAML [[[[")))
	w = httptest.NewRecorder()
	s.handlePipelineDetail(w, req)
	if w.Code != 400 {
		t.Fatalf("bad yaml should 400 got %d", w.Code)
	}
	// traversal blocked
	req = httptest.NewRequest("GET", "/api/pipelines/../secret.yaml", nil)
	w = httptest.NewRecorder()
	s.handlePipelineDetail(w, req)
	if w.Code != 400 {
		t.Fatalf("traversal %d", w.Code)
	}
	// method not allowed
	req = httptest.NewRequest("DELETE", "/api/pipelines/a.yaml", nil)
	w = httptest.NewRecorder()
	s.handlePipelineDetail(w, req)
	if w.Code != 405 {
		t.Fatalf("method %d", w.Code)
	}
}

func TestHandlePipelinesList(t *testing.T) {
	pDir := t.TempDir()
	os.WriteFile(filepath.Join(pDir, "good.yaml"), []byte("format_version: \"0.2\"\npipeline:\n  name: good\n  input: {}\n  steps: []\n"), 0o644)
	os.WriteFile(filepath.Join(pDir, "bad.yaml"), []byte("bad content"), 0o644)
	s := NewServer(t.TempDir(), pDir, t.TempDir())
	req := httptest.NewRequest("GET", "/api/pipelines", nil)
	w := httptest.NewRecorder()
	s.handlePipelines(w, req)
	if w.Code != 200 {
		t.Fatalf("pipelines %d", w.Code)
	}
}

func TestHandlePluginInstallDeps(t *testing.T) {
	s := NewServer(t.TempDir(), t.TempDir(), t.TempDir())
	// wrong method
	req := httptest.NewRequest("GET", "/api/plugins-install-deps?id=x", nil)
	w := httptest.NewRecorder()
	s.handlePluginInstallDeps(w, req)
	if w.Code != 405 {
		t.Fatalf("method %d", w.Code)
	}
	// missing id
	req = httptest.NewRequest("POST", "/api/plugins-install-deps", nil)
	w = httptest.NewRecorder()
	s.handlePluginInstallDeps(w, req)
	if w.Code != 400 {
		t.Fatalf("missing id %d", w.Code)
	}
	// bad id with slash
	req = httptest.NewRequest("POST", "/api/plugins-install-deps?id=bad/id", nil)
	w = httptest.NewRecorder()
	s.handlePluginInstallDeps(w, req)
	if w.Code != 400 {
		t.Fatalf("bad id %d", w.Code)
	}
	// not found
	req = httptest.NewRequest("POST", "/api/plugins-install-deps?id=unknown", nil)
	w = httptest.NewRecorder()
	s.handlePluginInstallDeps(w, req)
	if w.Code != 404 {
		t.Fatalf("not found %d", w.Code)
	}
}

func TestHandleValidateAndPlan(t *testing.T) {
	s := NewServer(t.TempDir(), t.TempDir(), t.TempDir())
	yaml := "format_version: \"0.2\"\npipeline:\n  name: t\n  input: {}\n  steps: []\n"
	for _, path := range []string{"/api/validate", "/api/plan/pipeline"} {
		// wrong method
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		if path == "/api/validate" {
			s.handleValidatePipeline(w, req)
		} else {
			s.handlePlanPipeline(w, req)
		}
		if w.Code != 405 {
			t.Fatalf("%s GET should 405 got %d", path, w.Code)
		}
		// valid POST
		req = httptest.NewRequest("POST", path, bytes.NewReader([]byte(yaml)))
		w = httptest.NewRecorder()
		if path == "/api/validate" {
			s.handleValidatePipeline(w, req)
		} else {
			s.handlePlanPipeline(w, req)
		}
		if w.Code != 200 {
			t.Fatalf("%s POST %d %s", path, w.Code, w.Body.String())
		}
		// bad yaml
		req = httptest.NewRequest("POST", path, bytes.NewReader([]byte("bad: [")))
		w = httptest.NewRecorder()
		if path == "/api/validate" {
			s.handleValidatePipeline(w, req)
		} else {
			s.handlePlanPipeline(w, req)
		}
		if w.Code != 400 {
			t.Fatalf("%s bad %d", path, w.Code)
		}
	}
}

func TestHandlePlanWithSteps(t *testing.T) {
	s := NewServer(t.TempDir(), t.TempDir(), t.TempDir())
	yaml := "format_version: \"0.2\"\npipeline:\n  name: t\n  foreach: input.items\n  input:\n    items: [1,2]\n  steps:\n    - id: a\n      plugin: fake/a\n    - id: b\n      plugin: fake/b\n      after_foreach: true\n      when:\n        path: steps.a.ok\n        op: truthy\n"
	req := httptest.NewRequest("POST", "/api/plan/pipeline", bytes.NewReader([]byte(yaml)))
	w := httptest.NewRecorder()
	s.handlePlanPipeline(w, req)
	if w.Code != 200 {
		t.Fatalf("plan with foreach %d %s", w.Code, w.Body.String())
	}
}

func TestNetworkJSONAndBlocked(t *testing.T) {
	if len(networkJSON(nil)) != 0 {
		t.Error("empty")
	}
	if blockedReason("test") == "" {
		t.Error("blocked")
	}
}

func TestSummarizeAndClone(t *testing.T) {
	m := map[string]interface{}{"a": 1}
	c := cloneSummary(m)
	if c["a"] != 1 {
		t.Error("clone")
	}
	acc := newRunSummary()
	if acc.status != "running" {
		t.Error("running")
	}
	acc.add(eventMetaOf(map[string]interface{}{"type": "run_start", "pipeline": "p", "ts": "2020-01-01"}))
	if acc.pipeline != "p" {
		t.Error("pipeline")
	}
}

func TestSecureContainedPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := secureContainedPath(dir, "../escape.yaml"); err == nil {
		t.Error("escape should fail")
	}
	if _, err := secureContainedPath(dir, "ok.yaml"); err != nil {
		t.Fatal(err)
	}
}
