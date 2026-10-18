//go:build integration

// Интеграционный тир. Тест НЕ должен попадать в обычный `go test ./...`:
//
//	он зовёт Python и порождает отдельный процесс на каждый шипленный плагин.
//	В юнит-пакете internal/core он занимал 168 с из 185 с всего пакета —
//	internal/core был самым долгим пакетом windows-джоба, и именно на нём
//	зависание проявлялось недетерминированно (один и тот же коммит: 22/22
//	за 387 с, затем висяк). Пределы времени тут не лечат: виноват процесс,
//	который не вернул управление.
//
//	Запуск отдельно, со своим бюджетом:
//	  go test -tags integration -run TestPluginTestShippedPlugins ./internal/core
package plugintest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPluginTestShippedPlugins — все шипленные плагины проходят свои
// plugin.test.yaml.
func TestPluginTestShippedPlugins(t *testing.T) {
	requirePython(t)
	// поддержка и плоской (plugins/<id>) и новой иерархии (plugins/official/<id>, plugins/community/<id>)
	patterns := []string{
		filepath.Join("..", "..", "plugins", "*"),
		filepath.Join("..", "..", "plugins", "*", "*"),
	}
	var dirs []string
	for _, pat := range patterns {
		m, _ := filepath.Glob(pat)
		dirs = append(dirs, m...)
	}
	if len(dirs) == 0 {
		t.Fatal("не найдены шипленные плагины")
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "plugin.yaml")); err != nil {
			continue // official/, community/ — не плагины
		}
		passed, failed, err := RunPluginTests(dir, "", true)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if failed != 0 {
			t.Fatalf("%s: %d тестов упали (passed=%d)", dir, failed, passed)
		}
	}
}
