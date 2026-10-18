package mcp

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/registry"
)

// вырезано из server.go 247-410 — без смены логики, только файл
// multiEngine — резолв плагинов по нескольким корням (песочница Фаза 4.4).
type multiEngine struct {
	dirs    []string
	workDir string
}

func parseManifestBytes(raw []byte) (*pipeline.Manifest, error) {
	var m pipeline.Manifest
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&m); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("manifest: несколько YAML-документов")
		}
		return nil, err
	}
	return &m, nil
}

func (m *multiEngine) pluginRoots() []string {
	roots := append([]string{}, m.dirs...)
	if m.workDir != "" {
		roots = append(roots, m.workDir)
	}
	return roots
}

func (m *multiEngine) pluginPathAllowed(path string) bool {
	for _, root := range m.pluginRoots() {
		if pathWithinRoot(root, path) {
			return true
		}
	}
	return false
}

func (m *multiEngine) checkPluginDir(path string) error {
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: plugin %q содержит NUL", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: plugin %q не разрешается", path)
	}
	if !m.pluginPathAllowed(abs) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: plugin %q вне --plugins/--workdir", path)
	}
	resolved, err := resolvePathForContainment(abs)
	if err != nil || !m.pluginPathAllowed(resolved) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: plugin %q уходит через symlink/junction", path)
	}
	manifest := filepath.Join(abs, "plugin.yaml")
	if _, err := os.Lstat(manifest); err == nil && !pathWithinRoot(abs, manifest) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: plugin %q manifest уходит через symlink/junction", path)
	}
	return nil
}

func (m *multiEngine) checkPluginRef(ref string) error {
	if strings.Contains(ref, "..") {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: путь %q выходит за корни плагинов", ref)
	}
	if (strings.HasPrefix(ref, "/") || (filepath.Separator == '\\' && strings.HasPrefix(ref, `\`))) && !filepath.IsAbs(ref) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: %q вне --plugins/--workdir", ref)
	}
	if !registry.IsLocalRef(ref) {
		var boundaryErr error
		for _, root := range m.pluginRoots() {
			dir, err := registry.RefToDir(ref, root)
			if err != nil {
				continue
			}
			if err := m.checkPluginDir(dir); err != nil {
				boundaryErr = err
				continue
			}
			return nil
		}
		return boundaryErr
	}
	path := ref
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, `\`) {
		path = filepath.Join(m.workDir, path)
	}
	return m.checkPluginDir(path)
}

func (m *multiEngine) loadLocal(ref string) (*pipeline.Manifest, error) {
	path := ref
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, `\`) {
		path = filepath.Join(m.workDir, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := m.checkPluginDir(path); err != nil {
		return nil, err
	}
	eng := core.NewEngine()
	eng.PluginsDir = m.workDir
	return eng.LoadManifest(path)
}

func (m *multiEngine) LoadManifest(ref string) (*pipeline.Manifest, error) {
	if pipeline.IsBuiltin(ref) {
		return &pipeline.Manifest{ID: "core/human_gate", Version: pipeline.PlatformAPI}, nil
	}
	if pipeline.IsBuiltinNamespace(ref) {
		return nil, fmt.Errorf("неизвестный встроенный модуль: %s", ref)
	}
	if registry.IsLocalRef(ref) {
		if err := m.checkPluginRef(ref); err != nil {
			return nil, err
		}
		return m.loadLocal(ref)
	}
	lastErr := fmt.Errorf("плагин %q не найден", ref)
	var boundaryErr error
	for _, dir := range m.dirs {
		candidate, err := registry.RefToDir(ref, dir)
		if err != nil {
			lastErr = err
			continue
		}
		if err := m.checkPluginDir(candidate); err != nil {
			boundaryErr = err
			continue
		}
		eng := core.NewEngine()
		eng.PluginsDir = dir
		if manifest, err := eng.LoadManifest(candidate); err == nil {
			return manifest, nil
		} else {
			lastErr = err
		}
	}
	candidate, err := registry.RefToDir(ref, m.workDir)
	if err == nil {
		if err := m.checkPluginDir(candidate); err != nil {
			boundaryErr = err
		} else {
			eng := core.NewEngine()
			eng.PluginsDir = m.workDir
			if manifest, err := eng.LoadManifest(candidate); err == nil {
				return manifest, nil
			} else {
				lastErr = err
			}
		}
	}
	if boundaryErr != nil {
		return nil, boundaryErr
	}
	return nil, lastErr
}

