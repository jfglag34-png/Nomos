// Package changelog проверяет структуру CHANGELOG.md.
//
// Зачем он нужен: структура ченджлога — это единственное место в репозитории,
// где ошибка не ломает сборку, но молча выбрасывает написанное. Код ошибки
// ловит гейт errcodes, битый YAML — валидатор, а вот потерянный заголовок
// раздела или запись, не вставленная в файл, выглядят ровно так же, как
// «никто ничего не менял».
//
// Конкретный случай, который закрывает этот пакет: правка через скрипт
// заменила строку `## Unreleased` на текст новой секции, и заголовок версии
// исчез, а следующая правка уже не нашла что заменять — запись про валидатор
// не попала в файл вообще, и никто этого не заметил.
//
// Проверки намеренно структурные, а не смысловые: «каждый подраздел лежит под
// разделом версии», «раздел Unreleased на месте», «хотя бы одна версия
// описана». Смысл проверять нечем — какие записи обязаны быть в ченджлоге,
// знает только человек, который их пишет.
package changelog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName — имя файла ченджлога в корне репозитория.
const FileName = "CHANGELOG.md"

// CheckRepo — проверяет структуру CHANGELOG.md.
//
// Возвращает краткий отчёт и ошибку, если что-то сломано. Файл отсутствует —
// это тоже поломка: проект без ченджлога невозможно выпустить осмысленно.
func CheckRepo(repo string) (string, error) {
	path := filepath.Join(repo, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: прочитать не удалось: %w", FileName, err)
	}
	return Check(string(raw))
}

// Check — проверяет структуру текста ченджлога.
func Check(text string) (string, error) {
	lines := strings.Split(text, "\n")

	var problems []string
	section := ""    // текущий раздел версии (## ...)
	subsection := "" // текущий подраздел (### ...)
	sections := 0    // сколько разделов версий
	subsections := 0 // сколько подразделов
	hasUnreleased := false
	hasVersion := false
	inFence := false // внутри блока кода — там строки вида ## не считаются

	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "# "):
			// Заголовок документа — разделы версий ниже относятся к нему.
			section, subsection = "", ""
		case strings.HasPrefix(trimmed, "## "):
			sections++
			section = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			subsection = ""
			if section == "Unreleased" {
				hasUnreleased = true
			}
			if section != "Unreleased" {
				hasVersion = true
			}
		case strings.HasPrefix(trimmed, "### "):
			subsections++
			subsection = strings.TrimSpace(strings.TrimPrefix(trimmed, "### "))
			if section == "" {
				problems = append(problems, fmt.Sprintf(
					"строка %d: подраздел %q без раздела версии над ним — заголовок раздела, вероятно, потерян",
					i+1, subsection))
			}
		}
	}

	if !hasUnreleased {
		problems = append(problems, "нет раздела `## Unreleased`")
	}
	if !hasVersion {
		problems = append(problems, "нет ни одного раздела версии (## 0.x.0)")
	}
	if sections == 0 && len(problems) == 0 {
		problems = append(problems, "разбор пустой — проверка сломана, а не прошла")
	}

	if len(problems) > 0 {
		return "", fmt.Errorf("%s: %s", FileName, strings.Join(problems, "; "))
	}
	return fmt.Sprintf("структура ченджлога: разделов %d, подразделов %d", sections, subsections), nil
}
