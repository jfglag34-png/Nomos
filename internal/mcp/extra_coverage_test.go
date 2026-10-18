package mcp

import (
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
)

func TestCheckPipelineSafety(t *testing.T) {
	s, _ := newTestServer(t, nil)
	pf := &pipeline.PipelineFile{
		Pipeline: pipeline.Pipeline{
			Name: "n",
			Steps: []pipeline.Step{
				{ID: "bad", Plugin: "../../../etc/passwd"},
			},
		},
	}
	if err := s.checkPipelineSafety(pf); err == nil {
		t.Error("should block traversal")
	}
	pf.Pipeline.Steps[0].Plugin = "ok/plugin"
	if err := s.checkPipelineSafety(pf); err != nil {
		t.Fatalf("ok plugin %v", err)
	}
}

func newTestServer(t *testing.T, opts *Options) (*Server, func()) {
	t.Helper()
	dir := t.TempDir()
	if opts == nil {
		opts = &Options{PluginsDirs: []string{dir}, WorkDir: dir, RunsDir: t.TempDir()}
	}
	s, err := NewServer(*opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, func() {}
}

func TestLoadPipelineValidation(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// empty args
	if _, err := s.loadPipeline(map[string]interface{}{}); err == nil {
		t.Error("empty should fail")
	}
}

func TestValidateArgs(t *testing.T) {
	s, _ := newTestServer(t, nil)
	pf, issues, err := s.validateArgs(map[string]interface{}{"yaml": "format_version: \"0.2\"\npipeline:\n  name: t\n  input: {}\n  steps: []\n"})
	if err != nil {
		t.Fatal(err)
	}
	if pf == nil {
		t.Fatal("pf nil")
	}
	_ = issues
}

func TestToolPlanAndValidate(t *testing.T) {
	s, _ := newTestServer(t, nil)
	_, _, err := s.toolValidate(map[string]interface{}{})
	if err == nil {
		t.Error("missing yaml should error")
	}
	_, _, err = s.toolPlan(map[string]interface{}{"yaml": "format_version: \"0.2\"\npipeline:\n  name: t\n  input: {}\n  steps: []\n"})
	if err != nil {
		t.Fatalf("toolPlan %v", err)
	}
}

func TestToolListPlugins(t *testing.T) {
	s, _ := newTestServer(t, nil)
	res, isErr, err := s.toolListPlugins(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if isErr {
		t.Error("should not error")
	}
	_ = res
}

func TestMultiEngineParse(t *testing.T) {
	m, err := parseManifestBytes([]byte("id: test\nversion: \"0.1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "test" {
		t.Error("id")
	}
	_, err = parseManifestBytes([]byte("bad: ["))
	if err == nil {
		t.Error("bad yaml")
	}
	_, err = parseManifestBytes([]byte("id: a\n---\nsecond: doc"))
	if err == nil {
		t.Error("multi doc should fail")
	}
}
