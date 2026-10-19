package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
)

func TestComposeFifteenPluginsAndSave(t *testing.T) {
	s := testServer(t)
	stages := []interface{}{}
	for stage := 0; stage < 5; stage++ {
		steps := []interface{}{}
		for branch := 0; branch < 3; branch++ {
			id := fmt.Sprintf("p_%d_%d", stage, branch)
			writeFakePlugin(t, s.pluginsDirs[0], id, nil, map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}})
			steps = append(steps, map[string]interface{}{"id": id, "plugin": filepath.Join(s.pluginsDirs[0], id)})
		}
		stages = append(stages, map[string]interface{}{"steps": steps})
	}
	args := map[string]interface{}{"name": "long_chain", "every_minutes": float64(7), "stages": stages, "save": true}
	text, _, e := s.toolCompose(args)
	if e != nil {
		t.Fatal(e)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(text), &result)
	if result["ok"] != true || result["activated"] != false {
		t.Fatal(text)
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(result["yaml"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Pipeline.Steps) != 15 {
		t.Fatal("step count")
	}
	for i, st := range pf.Pipeline.Steps {
		if st.ParallelGroup != fmt.Sprintf("stage_%03d", i/3+1) {
			t.Fatal("barrier grouping")
		}
	}
	for _, key := range []string{"pipeline_path", "fleet_path"} {
		if _, err = os.Stat(result[key].(string)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, e = s.toolCompose(args); e == nil {
		t.Fatal("overwrite allowed")
	}
}
func TestComposeRejectsTraversalAndInterval(t *testing.T) {
	s := testServer(t)
	for _, args := range []map[string]interface{}{
		{"name": "../escape", "every_minutes": float64(5)},
		{"name": "good", "every_minutes": float64(0)},
		{"name": "good", "every_minutes": float64(1.5)},
	} {
		if _, _, e := s.toolCompose(args); e == nil {
			t.Fatal("bad args accepted")
		}
	}
}

func TestComposeNetworkDraftDoesNotEnableMCPExecution(t *testing.T) {
	s := testServer(t)
	ref := writePolicyPlugin(t, s.pluginsDirs[0], "network_connector", []map[string]interface{}{{"any_host": true, "port": 443}}, nil)
	args := map[string]interface{}{"name": "network_draft", "every_minutes": float64(5), "network": "allow", "stages": []interface{}{map[string]interface{}{"steps": []interface{}{map[string]interface{}{"id": "request", "plugin": ref}}}}}
	text, _, e := s.toolCompose(args)
	if e != nil {
		t.Fatal(e)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(text), &result)
	if result["ok"] != true || result["mcp_network_policy_allowed"] != false || result["activated"] != false {
		t.Fatal(text)
	}
	if _, e := s.loadPipeline(map[string]interface{}{"yaml": result["yaml"]}); e == nil {
		t.Fatal("network execution policy bypassed")
	}
}

func TestComposeNetworkDraftStillChecksFilePaths(t *testing.T) {
	s := testServer(t)
	ref := writePolicyPlugin(t, s.pluginsDirs[0], "network_file_connector", []map[string]interface{}{{"any_host": true, "port": 443}}, map[string]interface{}{"file": map[string]interface{}{"type": "string", "format": "file_ref", "from": "input.file"}})
	// /etc/passwd is NOT an absolute path on Windows. Use actual native paths
	// and real files so this test checks confinement rather than Unix spelling.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	inside := filepath.Join(s.workDir, "inside.txt")
	parent := filepath.Join(filepath.Dir(s.workDir), "parent.txt")
	for _, path := range []string{outside, inside, parent} {
		if e := os.WriteFile(path, []byte("fixture"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		name, path string
		allowed    bool
	}{
		{"outside_absolute", outside, false},
		{"parent_traversal", filepath.Join("..", "parent.txt"), false},
		{"inside_absolute", inside, true},
		{"inside_relative", "inside.txt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]interface{}{"name": "network_file_draft", "every_minutes": float64(5), "network": "allow", "input": map[string]interface{}{"file": tc.path}, "stages": []interface{}{map[string]interface{}{"steps": []interface{}{map[string]interface{}{"id": "request", "plugin": ref}}}}}
			text, _, e := s.toolCompose(args)
			if !tc.allowed {
				if e == nil {
					t.Fatal("file path safety bypassed")
				}
				if !strings.Contains(e.Message, "E_FILE_REF_OUTSIDE_ROOT") {
					t.Fatalf("unexpected refusal: %v", e)
				}
				return
			}
			if e != nil {
				t.Fatalf("in-workdir file refused: %v", e)
			}
			var result map[string]interface{}
			if err := json.Unmarshal([]byte(text), &result); err != nil {
				t.Fatal(err)
			}
			if result["ok"] != true {
				t.Fatal(text)
			}
		})
	}
}
