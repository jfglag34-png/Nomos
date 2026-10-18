package api

import (
	"net/http"
	"os"
	"strings"

	"github.com/wykserdex/wedra/internal/plugin"
)

// handleTrust — как в этой установке устроено доверие и куда положить файл.
//
// Зачем отдельный эндпоинт, когда можно было добавить поле в /api/plugins:
// доверие — свойство УСТАНОВКИ, а не плагина. Оно одинаково для всех, и
// спрашивать его на каждой карточке — значит отправлять в браузер одно и то же
// десять раз.
//
// Ответ отвечает на вопрос, который иначе остаётся без ответа: «плагин не
// доверен, что делать». На платформе с изолятором (Linux) ответ «он запустится
// в bwrap, доверять не нужно». На Windows и macOS изолятора нет, и единственный
// способ запустить СВОЙ код — внести его хэш в конфиг оператора. Без пути к
// конфигу человек либо не найдёт файл, либо положит его в рабочий каталог, откуда
// его намеренно не читают.
func (s *Server) handleTrust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "сведения о доверии только GET", "code": "E_METHOD_NOT_ALLOWED",
		})
		return
	}
	path := strings.TrimSpace(s.TrustConfigPath)
	out := map[string]interface{}{
		"config_path": path,
		"file":        plugin.TrustConfigFile,
		"search_order": []string{
			"--trust-config=<путь>",
			"WEDRA_TRUST_CONFIG=<путь>",
			"каталог бинаря: wedra-trust.yaml рядом с wedra",
		},
		// Рабочий каталог назван отдельным пунктом не для красоты: если файл
		// лежит там и НЕ выбран, CLI печатает предупреждение, а страница
		// обязана объяснить то же самое. Молча проигнорированный конфиг — это
		// «оператор решил, что доверяет, а на деле нет».
		"cwd_not_read": true,
	}
	// Windows UX: AppContainer нет, доверять можно только через wedra-trust.yaml
	if isWindows() {
		out["windows_note"] = "На Windows изолятор (bwrap) недоступен — внешний код запускается только если его хэш в wedra-trust.yaml. Положите файл рядом с wedra.exe или задайте --trust-config."
		out["example_path"] = "%ProgramData%\\wedra\\wedra-trust.yaml или C:\\wedra\\wedra-trust.yaml"
	}
	if path == "" {
		out["configured"] = false
		how := "задайте --trust-config=<путь> или переменную WEDRA_TRUST_CONFIG"
		if isWindows() {
			how += " (пример: --trust-config=\"C:\\wedra\\wedra-trust.yaml\")"
		}
		out["how_to_configure"] = how
	} else {
		out["configured"] = true
		if _, err := os.Stat(path); err == nil {
			out["exists"] = true
		} else {
			out["exists"] = false
			msg := "создайте файл: " + path
			if isWindows() {
				msg += " (запустите PowerShell от админа: New-Item -Path \"" + path + "\" -ItemType File)"
			}
			out["how_to_create"] = msg
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func isWindows() bool {
	return strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") || strings.HasSuffix(strings.ToLower(os.Getenv("COMSPEC")), "cmd.exe") || strings.Contains(strings.ToLower(os.Getenv("ProgramData")), "programdata")
}
