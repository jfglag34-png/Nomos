package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"github.com/wykserdex/wedra/internal/pipeline"
)

func (s *Server) checkPluginRef(ref string) error {
	if pipeline.IsBuiltin(ref) {
		return nil
	}
	if pipeline.IsBuiltinNamespace(ref) {
		return fmt.Errorf("E_PLUGIN_LOAD: неизвестный встроенный модуль: %s", ref)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: путь %q выходит за корни плагинов", ref)
	}
	if s.multi != nil {
		return s.multi.checkPluginRef(ref)
	}
	isAbs := filepath.IsAbs(ref) || strings.HasPrefix(ref, "/")
	if isAbs {
		abs := ref
		if !filepath.IsAbs(abs) {
			return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: %q вне --plugins/--workdir", ref)
		}
		abs, _ = filepath.Abs(abs)
		for _, root := range append(append([]string{}, s.pluginsDirs...), s.workDir) {
			if pathWithinRoot(root, abs) {
				return nil
			}
		}
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: %q вне --plugins/--workdir", ref)
	}
	return nil
}

func (s *Server) checkPathInWorkdir(p string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: path %q содержит NUL", p)
	}
	candidate := p
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(s.workDir, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil || !pathWithinRoot(s.workDir, abs) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: path %q вне --workdir", p)
	}
	return nil
}

func resolvePathForContainment(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current = filepath.Clean(current)
	for links := 0; links < 255; links++ {
		probe := current
		suffix := []string{}
		for {
			if target, linkErr := os.Readlink(probe); linkErr == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(probe), target)
				}
				current = filepath.Join(append([]string{target}, suffix...)...)
				break
			}
			if info, statErr := os.Lstat(probe); statErr == nil {
				if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
					return "", fmt.Errorf("reparse point: %s", probe)
				}
			} else if !os.IsNotExist(statErr) {
				return "", statErr
			}
			if resolved, evalErr := filepath.EvalSymlinks(probe); evalErr == nil {
				for _, part := range suffix {
					resolved = filepath.Join(resolved, part)
				}
				return filepath.Clean(resolved), nil
			}
			parent := filepath.Dir(probe)
			if parent == probe {
				return "", fmt.Errorf("path cannot be resolved: %s", current)
			}
			suffix = append([]string{filepath.Base(probe)}, suffix...)
			probe = parent
		}
	}
	return "", fmt.Errorf("too many links: %s", current)
}

func pathWithinRoot(root, path string) bool {
	rootAbs, err := resolvePathForContainment(root)
	if err != nil {
		return false
	}
	pathAbs, err := resolvePathForContainment(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func foreachInputKey(foreach, item string) string {
	if foreach == "" {
		return ""
	}
	if item == "" {
		return "item"
	}
	return item
}

func fileRefSourceDynamic(pf *pipeline.PipelineFile, st *pipeline.Step, source string) bool {
	if !strings.HasPrefix(source, "input.") {
		return true
	}
	key := strings.TrimPrefix(source, "input.")
	if key == "" || strings.Contains(key, ".") {
		return true
	}
	if pf.Pipeline.Foreach != "" && key == foreachInputKey(pf.Pipeline.Foreach, pf.Pipeline.ForeachItem) {
		return true
	}
	if st != nil && st.Foreach != "" && key == foreachInputKey(st.Foreach, st.ForeachItem) {
		return true
	}
	for i := range pf.Pipeline.Steps {
		step := &pf.Pipeline.Steps[i]
		if step.Foreach != "" && key == foreachInputKey(step.Foreach, step.ForeachItem) {
			return true
		}
	}
	return false
}

