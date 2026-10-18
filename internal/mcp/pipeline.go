package mcp

import (
	"fmt"
	"github.com/wykserdex/wedra/internal/pipeline"
	"net"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) checkPipelineSafety(pf *pipeline.PipelineFile) error {
	for i := range pf.Pipeline.Steps {
		st := &pf.Pipeline.Steps[i]
		if pipeline.IsBuiltin(st.Plugin) {
			continue
		}
		m, err := s.multi.LoadManifest(st.Plugin)
		if err != nil {
			if strings.HasPrefix(err.Error(), "E_PLUGIN_OUTSIDE_ROOT") {
				return err
			}
			continue
		}
		for _, permission := range m.Permissions.Network {
			if permission.AnyHost {
				return fmt.Errorf("E_NETWORK_DENIED: плагин %s заявил any_host в MCP", m.ID)
			}
			host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(permission.Host), "."))
			if host == "" {
				return fmt.Errorf("E_NETWORK_DENIED: пустой host в permissions плагина %s", m.ID)
			}
			if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
				return fmt.Errorf("E_NETWORK_DENIED: host %q запрещён в MCP", permission.Host)
			}
			if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
				return fmt.Errorf("E_NETWORK_DENIED: private/loopback host %q запрещён в MCP", permission.Host)
			}
		}
		for name, port := range m.Input {
			if port.Format != "file_ref" {
				continue
			}
			source := pipeline.PortSource(name, port, st)
			if fileRefSourceDynamic(pf, st, source) {
				return fmt.Errorf("E_FILE_REF_UNCHECKED: плагин %s, порт %s, источник %q", m.ID, name, source)
			}
			key := strings.TrimPrefix(source, "input.")
			raw, ok := pf.Pipeline.Input[key]
			if !ok {
				continue
			}
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("E_FILE_REF_UNCHECKED: input.%s должен быть строкой", key)
			}
			if err := s.checkPathInWorkdir(value); err != nil {
				return fmt.Errorf("E_FILE_REF_OUTSIDE_ROOT: input.%s: %w", key, err)
			}
		}
	}
	return nil
}

// loadPipeline — yaml или path (ровно один), с проверкой песочницы.
func (s *Server) loadPipeline(args map[string]interface{}) (*pipeline.PipelineFile, error) {
	yamlStr, _ := args["yaml"].(string)
	pathStr, _ := args["path"].(string)
	if yamlStr != "" && pathStr != "" {
		return nil, fmt.Errorf("укажите только один: yaml или path")
	}
	var raw []byte
	if pathStr != "" {
		if err := s.checkPathInWorkdir(pathStr); err != nil {
			return nil, err
		}
		full := pathStr
		if !filepath.IsAbs(full) {
			full = filepath.Join(s.workDir, pathStr)
		}
		var err error
		raw, err = os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("path %q: %w", pathStr, err)
		}
	} else if yamlStr != "" {
		raw = []byte(yamlStr)
	} else {
		return nil, fmt.Errorf("нужен yaml или path")
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	for _, st := range pf.Pipeline.Steps {
		if err := s.checkPluginRef(st.Plugin); err != nil {
			return nil, err
		}
	}
	if err := s.checkPipelineSafety(pf); err != nil {
		return nil, err
	}
	return pf, nil
}

// validate — общий путь для validate/run/plan: загрузка + ValidateIssues.
func (s *Server) validateArgs(args map[string]interface{}) (*pipeline.PipelineFile, []pipeline.Issue, error) {
	pf, err := s.loadPipeline(args)
	if err != nil {
		return nil, nil, err
	}
	issues := pipeline.ValidateIssues(pf, s.multi)
	return pf, issues, nil
}

// Serve — главный цикл stdio.
//
// Разбор сообщения больше не может завершить сессию: битая строка и батч-массив
// получают ответ (-32700 / -32600), а цикл продолжается. Раньше обе ситуации
// возвращались как ошибка чтения, и сервер умирал, отдав клиенту код выхода 0.
