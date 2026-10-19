package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
