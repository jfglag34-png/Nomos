package plugintest

import (
	"os/exec"
	"testing"
)

func requirePython(t *testing.T) {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("python не найден — пропускаю интеграционный тест")
}
