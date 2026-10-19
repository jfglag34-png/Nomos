package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
	"gopkg.in/yaml.v3"
)

// compose_pipeline constructs sequential stages with a barrier after each parallel stage.
// It generates artifacts, never executes code or enables unattended approval.
func (s *Server) toolCompose(args map[string]interface{}) (string, bool, *RPCError) {
	name, _ := args["name"].(string)
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(name) {
		return "", false, rpcErr("", "name must be 1..64 alphanumeric, underscore or hyphen")
	}
	n, ok := args["every_minutes"].(float64)
	if !ok || n < 1 || n > 525600 || n != float64(int(n)) {
		return "", false, rpcErr("", "every_minutes must be an integer 1..525600")
	}
	timeout, _ := args["timeout"].(string)
	if timeout == "" {
		timeout = "30m"
	}
	if d, e := time.ParseDuration(timeout); e != nil || d <= 0 {
		return "", false, rpcErr("", "invalid timeout")
	}
	stages, ok := args["stages"].([]interface{})
	if !ok || len(stages) == 0 || len(stages) > 256 {
		return "", false, rpcErr("", "stages must contain 1..256 stages")
	}
	steps := []interface{}{}
	for i, raw := range stages {
		stage, ok := raw.(map[string]interface{})
		if !ok {
			return "", false, rpcErr("", "stage must be an object")
		}
		list, ok := stage["steps"].([]interface{})
		if !ok || len(list) == 0 || len(list) > pipeline.MaxParallelWidth {
			return "", false, rpcErr("", "each stage needs 1..32 steps")
		}
		for _, rawStep := range list {
			st, ok := rawStep.(map[string]interface{})
			if !ok {
				return "", false, rpcErr("", "step must be an object")
			}
			copy := map[string]interface{}{}
			for k, v := range st {
				copy[k] = v
			}
			if _, exists := copy["parallel_group"]; exists {
				return "", false, rpcErr("", "parallel_group is assigned by stage; omit it")
			}
			if len(list) > 1 {
				copy["parallel_group"] = fmt.Sprintf("stage_%03d", i+1)
			}
			steps = append(steps, copy)
		}
	}
	if len(steps) > 256 {
		return "", false, rpcErr("", "maximum 256 steps")
	}
	input, _ := args["input"].(map[string]interface{})
	if input == nil {
		input = map[string]interface{}{}
	}
	raw, e := yaml.Marshal(map[string]interface{}{"format_version": "0.2", "pipeline": map[string]interface{}{"name": name, "input": input, "steps": steps}})
	if e != nil {
		return "", false, rpcErr("", e.Error())
	}
	_, issues, e := s.validateArgs(map[string]interface{}{"yaml": string(raw)})
	if e != nil {
		return "", false, rpcErr("", e.Error())
	}
	errs, _ := pipeline.SplitIssues(issues)
	result := map[string]interface{}{"ok": len(errs) == 0, "issues": issuesForJSON(issues), "yaml": string(raw), "step_count": len(steps), "stage_count": len(stages), "activated": false}
	if len(errs) > 0 {
		return toJSON(result), false, nil
	}
	// A relative draft path is intentionally not installed in the active service config.
	fleet, e := yaml.Marshal(map[string]interface{}{"concurrency": 6, "listen": "127.0.0.1:8770", "runs_dir": "var/fleet", "jobs": []interface{}{map[string]interface{}{"id": name, "file": name + ".pipeline.yaml", "every": fmt.Sprintf("%dm", int(n)), "timeout": timeout, "auto_approve": false}}})
	if e != nil {
		return "", false, rpcErr("", e.Error())
	}
	result["fleet_yaml"] = string(fleet)
	save, _ := args["save"].(bool)
	if save {
		// os.Root confines all writes, including when an attacker replaces a path with a symlink.
		root, e := os.OpenRoot(s.workDir)
		if e != nil {
			return "", false, rpcErr("", e.Error())
		}
		defer root.Close()
		pipelinePath := name + ".pipeline.yaml"
		fleetPath := name + ".fleet.yaml"
		// Never overwrite artifacts: use a new name to create a revised bundle.
		pf, e := root.OpenFile(pipelinePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return "", false, rpcErr("", e.Error())
		}
		if _, e = pf.Write(raw); e != nil {
			pf.Close()
			root.Remove(pipelinePath)
			return "", false, rpcErr("", e.Error())
		}
		if e = pf.Close(); e != nil {
			root.Remove(pipelinePath)
			return "", false, rpcErr("", e.Error())
		}
		ff, e := root.OpenFile(fleetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			root.Remove(pipelinePath)
			return "", false, rpcErr("", e.Error())
		}
		_, e = ff.Write(fleet)
		ce := ff.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			root.Remove(fleetPath)
			root.Remove(pipelinePath)
			return "", false, rpcErr("", e.Error())
		}
		result["pipeline_path"] = filepath.Join(s.workDir, pipelinePath)
		result["fleet_path"] = filepath.Join(s.workDir, fleetPath)
	}
	result["next_steps"] = "Review artifacts and required human gates. Operator merges job into active fleet.yaml and restarts service. Configure alert channels via environment. Paths resolve from fleet working directory. Never auto-approve on agent request."
	return toJSON(result), false, nil
}
