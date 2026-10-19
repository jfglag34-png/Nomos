package changelog

import (
	"strings"
	"testing"
)

// TestCheckAcceptsNormalChangelog — обычный файл обязан проходить.
func TestCheckAcceptsNormalChangelog(t *testing.T) {
	text := `# Changelog — WEDRA

## Unreleased

### Что-то сделано

- пункт

## 0.35.0 — в работе

### Ещё что-то

- пункт
`
	if _, err := Check(text); err != nil {
		t.Fatalf("обычный ченджлог не прошёл: %v", err)
	}
}

// TestCheckRejectsSubsectionWithoutSection — главный регрессионный случай.
//
// Правка скриптом заменила `## Unreleased` на текст секции, и все подразделы
// оказались без раздела версии над собой. Такое молча выглядит как «никто
// ничего не менял», поэтому проверка обязана ловить.
func TestCheckRejectsSubsectionWithoutSection(t *testing.T) {
	text := `# Changelog — WEDRA

### Гейт в демоне больше не висит молча

- пункт

### Аудит 1-2-3

- пункт
`
	_, err := Check(text)
	if err == nil {
		t.Fatal("подраздел без раздела версии должен давать ошибку")
	}
	if !strings.Contains(err.Error(), "без раздела версии") {
		t.Errorf("в ошибке нет указания на потерянный заголовок: %v", err)
	}
}

// TestCheckRequiresUnreleased — без раздела Unreleased структура сломана:
// непонятно, куда попадает новое.
func TestCheckRequiresUnreleased(t *testing.T) {
	text := `# Changelog — WEDRA

## 0.35.0 — в работе

### Что-то

- пункт
`
	_, err := Check(text)
	if err == nil {
		t.Fatal("без `## Unreleased` должна быть ошибка")
	}
	if !strings.Contains(err.Error(), "Unreleased") {
		t.Errorf("в ошибке нет упоминания Unreleased: %v", err)
	}
}

// TestCheckRequiresAVersionSection — ченджлог без единой версии бессмыслен.
func TestCheckRequiresAVersionSection(t *testing.T) {
	text := `# Changelog — WEDRA

## Unreleased

### Что-то

- пункт
`
	_, err := Check(text)
	if err == nil {
		t.Fatal("без раздела версии должна быть ошибка")
	}
}

// TestCheckIgnoresHeadingsInsideCodeFence — в блоке кода строка `## пример`
// это пример, а не раздел. Иначе пример в доке ломал бы ченджлог.
func TestCheckIgnoresHeadingsInsideCodeFence(t *testing.T) {
	text := "# Changelog — WEDRA\n\n## Unreleased\n\n### Секция\n\n```yaml\n## это не раздел\n### и это не подраздел\n```\n\n## 0.35.0 — в работе\n\n### Ок\n"
	if _, err := Check(text); err != nil {
		t.Fatalf("заголовки внутри блока кода были приняты за разделы: %v", err)
	}
}

// TestCheckRejectsEmptyInput — пустой файл это поломка, а не «всё хорошо».
func TestCheckRejectsEmptyInput(t *testing.T) {
	if _, err := Check(""); err == nil {
		t.Fatal("пустой ченджлог должен давать ошибку")
	}
}
