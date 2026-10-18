// Файл generate.go — второй слой пакета importer: ГЕНЕРАЦИЯ.
//
// Первый слой (Plan) читает чужой проект и отвечает на вопрос «что из этого
// можно взять». Этот файл берёт ImportPlan и создаёт на диске настоящий
// каталог плагина WEDRA.
//
// Главное, что этот слой НЕ делает: он не делает вид, что плагин работает.
// Импортёр умеет надёжно ровно три вещи — санитизировать имя, разложить
// зависимости по пинам и поставить чужой модуль на sys.path. Всё остальное
// (сигнатура функции донора, разбор её ответа, распределение по портам
// манифеста) требует знаний, которых у нас нет, и угадывать их — значит
// через неделю получить плагин, который падает с ошибкой, которую не с чем
// сопоставить. Поэтому всё нерешённое уходит в Result.Blockers и в текст
// README, а на диск попадает скелет с честной точкой входа для человека.
//
// Границы намеренные:
//
//   - никакого exec, сети и новых зависимостей: только запись файлов;
//   - отказ на непустой целевой папке, как и в internal/core/plugincreate.go —
//     импорт не должен дозаписывать поверх чужой работы;
//   - требования не пишутся частично: манифест с половиной зависимостей
//     выглядит готовым и падает позже, в рантайме, без внятной причины.

package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenerateOptions — что вызывающий может поправить руками. Пустой
// GenerateOptions даёт рабочий (валидный и тестируемый) результат: все
// значения, которые нельзя угадать, берутся из плана, а нерешённое попадает
// в Blockers.
type GenerateOptions struct {
	// PluginID — переопределить id из плана. Принимается только уже годный
	// id (^[a-z][a-z0-9_]*$): молча переименовать плагин, которого человек
	// назвал иначе, нельзя, а «починить» id можно и через Plan.
	PluginID string
	// Description — переопределить description. Описание донора само по себе
	// описывает не плагин, поэтому подставлять его стоит осознанно.
	Description string
}

// Result — что получилось на диске и что осталось нерешённым.
//
// Warnings и Blockers копируются из плана и дополняются решениями генерации,
// поэтому Result ничего не теряет по дороге: вызывающий печатает один список
// и получает полную картину, а не только то, что сломалось при записи.
type Result struct {
	// Dir — куда записали (то, что передали в Generate, приведённое к чистому
	// виду).
	Dir string
	// PluginID — id, записанный в plugin.yaml.
	PluginID string
	// Files — имена записанных файлов, относительные Dir, в порядке записи.
	Files []string
	// Warnings — «проверь, что так и задумано».
	Warnings []string
	// Blockers — «автоматический импорт не получился, нужен человек».
	Blockers []string
}

// generatedFile — файл, который мы собираемся записать. Содержимое готовится
// ДО создания каталога: ошибка сборки не должна оставить после себя папку с
// половиной файлов (то же решение, что в internal/core/plugincreate.go).
type generatedFile struct {
	name    string
	content string
}

// Generate создаёт каталог плагина по плану импорта.
//
// Порядок проверок задан один раз и каждая проверка отвечает на свой вопрос:
//
//	id → целевой каталог → содержимое файлов → создание каталога → запись.
//
// То есть отказ приходит ДО первой записи. Единственное исключение — сама
// запись файлов: сбой диска на середине оставит часть файлов, и об этом
// честно сообщает ошибка вместе со списком уже записанного.
func Generate(plan ImportPlan, destDir string, opts GenerateOptions) (Result, error) {
	var res Result

	if strings.TrimSpace(destDir) == "" {
		return res, fmt.Errorf("каталог плагина не указан")
	}
	dest := filepath.Clean(destDir)

	id, err := pluginIDForGeneration(plan, opts)
	if err != nil {
		return res, err
	}

	if err := checkDestDir(dest); err != nil {
		return res, err
	}

	res = planResult(plan, dest, id)
	skel := decide(plan, id, opts, &res)
	files := renderFiles(skel, res)

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return res, fmt.Errorf("не создался каталог %s: %w", dest, err)
	}
	for _, f := range files {
		target := filepath.Join(dest, f.name)
		if err := os.WriteFile(target, []byte(f.content), 0o644); err != nil {
			// Уже записанные файлы в Result.Files остаются: сообщение обязано
			// сказать, что на диске есть, иначе человек ищет «сломанный» плагин
			// там, где его нет.
			return res, fmt.Errorf("не записался %s в %s (записано до этого: %v): %w",
				f.name, dest, res.Files, err)
		}
		res.Files = append(res.Files, f.name)
	}
	return res, nil
}

// pluginIDForGeneration выбирает id и проверяет его безопасность.
//
// Санитизировать здесь нечего: Plan это уже сделал, а ImportPlan, собранная
// руками, может содержать что угодно. Тихий «почин» здесь опаснее отказа:
// плагин, названный не тем, о чём думал человек, попадёт в пайплайны под
// чужим именем, и найти его потом можно будет только перебором каталогов.
func pluginIDForGeneration(plan ImportPlan, opts GenerateOptions) (string, error) {
	id := strings.TrimSpace(opts.PluginID)
	if id == "" {
		id = strings.TrimSpace(plan.PluginID)
	}
	if id == "" {
		return "", fmt.Errorf(
			"в плане нет id плагина: Plan должен был его посчитать, " +
				"а ручная сборка ImportPlan обязана задать PluginID")
	}
	if !pluginIDRe.MatchString(id) {
		return "", fmt.Errorf(
			"id плагина %q не подходит: ожидается snake_case ^[a-z][a-z0-9_]*$ "+
				"(точки и слэши в id недопустимы — иначе плагин выйдет за пределы своего каталога)",
			id)
	}
	return id, nil
}

// checkDestDir отвечает на вопрос «можно ли сюда писать» и ничего не создаёт.
//
// Непустой каталог — отказ, а не дописывание: рядом может лежать плагин
// человека, а сгенерированный main.py его перезаписал бы молча.
func checkDestDir(dest string) error {
	info, err := os.Stat(dest)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("%s — не каталог, а файл: плагин туда положить нельзя", dest)
	case err == nil:
		entries, readErr := os.ReadDir(dest)
		if readErr != nil {
			return fmt.Errorf("не читается каталог %s: %w", dest, readErr)
		}
		if len(entries) > 0 {
			return fmt.Errorf("папка %s не пуста — не перезаписываю", dest)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("каталог %s недоступен: %w", dest, err)
	}
	return nil
}

// planResult переносит план в результат и добавляет предупреждения, которые
// видны только при генерации.
func planResult(plan ImportPlan, dest, id string) Result {
	res := Result{
		Dir:      dest,
		PluginID: id,
		Warnings: append([]string(nil), plan.Warnings...),
		Blockers: append([]string(nil), plan.Blockers...),
	}
	// Имя каталога и id плагина — разные вещи, и это нормально: ядро ищет
	// плагин по пути, а реестр сверяет id с ИМЕНЕМ ЗАПИСИ. Но человек,
	// положивший каталог в plugins/, ожидает одного и того же — предупреждаем.
	if base := filepath.Base(dest); base != id {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"каталог называется %q, а id плагина %q — в реестре сверяется id с именем записи, а не с папкой; "+
				"если это не задумано, переименуйте папку в %s", base, id, id))
	}
	return res
}
