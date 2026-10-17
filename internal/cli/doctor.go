package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/wykserdex/wedra/internal/doctor"
)

// doctor: ответ на вопрос «я скачал бинарь, ничего не настроил, что мне
// сломано и что делать».
//
// Здесь осталось только то, что по определению принадлежит CLI: разбор флагов,
// выбор формата вывода и код выхода. Сама проба и отчёт живут в
// internal/doctor, и это не «вынесено для красоты»: магазин плагинов в GUI
// показывает те же данные, а internal/cli импортирует internal/api
// (см. internal/cli/gui.go) — значит импорт cli из api дал бы цикл и веб-интерфейс
// остался бы без данных doctor.
//
// Разбор флагов, тексты и коды выхода — 1:1 с тем, что было до переноса. Список
// допустимых флагов и код 2 на неизвестный флаг — часть контракта команды, их
// видно в скриптах, поэтому здесь ничего не «улучшено».
func RunDoctor(args []string) {
	asJSON := false
	pluginsDir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "--plugins="):
			pluginsDir = strings.TrimPrefix(a, "--plugins=")
		case a == "--plugins" && i+1 < len(args):
			i++
			pluginsDir = args[i]
		default:
			fmt.Printf("неизвестный флаг doctor %q\n", a)
			os.Exit(2)
		}
	}

	report := doctor.Run(pluginsDir)

	if asJSON {
		printJSON(report)
		if !report.OK {
			os.Exit(1)
		}
		return
	}
	// os.Stdout — здесь, а не внутри doctor: пакет ядра не должен печатать сам,
	// иначе его вывод нельзя ни перенаправить, ни показать в веб-интерфейсе.
	doctor.PrintHuman(os.Stdout, report)
	if !report.OK {
		os.Exit(1)
	}
}
