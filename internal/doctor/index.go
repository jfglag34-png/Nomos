package doctor

import (
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// indexState — ответ индекса пакетов. Три состояния, а не два, и это не
// педантизм: «нет на индексе» и «не удалось проверить» требуют от doctor
// противоположного поведения. Первое — можно сказать «ставьте из исходников».
// Второе — нельзя сказать ничего, кроме «проверь сам».
type indexState int

const (
	indexUnknown indexState = iota
	indexAbsent
	indexPresent
)

// indexLookup — проверка «есть ли такой пакет на PyPI».
//
// Именно проверка, а не установка: doctor по-прежнему ничего не ставит и
// ничего не запускает. Это HTTP GET одного JSON-индекса.
//
// Переменная, а не прямой вызов намеренно: она меняет ответ, по которому
// doctor решает, что человеку показать, а юнит-тесты doctor обязаны быть
// детерминированными и не ходить в сеть. Подменяет её только doctor_test.go
// (прежде doctor_test.go в internal/cli — то же самое, имя переменной сокращено
// вместе с переносом в пакет doctor, иначе читалось бы doctor.doctorIndexLookup).
var indexLookup = pypiIndexLookup

// pypiIndexLookup спрашивает индекс по имени пакета.
//
// Ровно GET https://pypi.org/pypi/<имя>/json: ответ 404 у pip означает
// «дистрибутива нет», и это единственный сигнал, на который здесь можно
// опереться. Код возврата pip для этого НЕ годится — проверено на этой
// машине: `pip index versions recon-ng` печатает «ERROR: No matching
// distribution found» в stderr и при этом завершается с кодом 0, то есть
// судить по коду возврата там, где pip отвечает в stderr, — значит
// выдумывать результат.
func pypiIndexLookup(name string) indexState {
	if name == "" {
		return indexUnknown
	}
	client := &http.Client{Timeout: 10 * time.Second}
	url := "https://pypi.org/pypi/" + neturl.PathEscape(name) + "/json"
	resp, err := client.Get(url)
	if err != nil {
		return indexUnknown
	}
	defer resp.Body.Close()
	// Тело не читаем целиком: нужен только код ответа, а индекс отдаёт
	// мегабайты JSON, из которых нужны две строки.
	switch {
	case resp.StatusCode == http.StatusOK:
		return indexPresent
	case resp.StatusCode == http.StatusNotFound:
		return indexAbsent
	default:
		// 500, 403, редирект на captcha и прочее — это «не знаю», а не «нет».
		return indexUnknown
	}
}

// indexAdvice — что сказать человеку, когда донора нет локально.
//
// Раньше здесь был безусловный `pip install <донор>`, и это ровно тот совет,
// который нельзя давать вслепую: для `recon-ng` и `spiderfoot` он был
// нерабочим. Теперь состояние индекса известно и отличено от «не знаю».
func indexAdvice(donor string, state indexState) string {
	switch state {
	case indexAbsent:
		return "пакета " + donor + " на PyPI нет — `pip install` не поможет; ставьте из исходников (git clone) или уберите плагин"
	case indexPresent:
		return "pip install " + donor
	default:
		return "не установлено; наличие " + donor + " на PyPI проверить не удалось — `pip install` может не сработать, сверьтесь вручную"
	}
}

// pathAdvice — что сказать, когда пакет стоит, а скрипта нет в PATH.
//
// Самое полезное здесь не «установите», а имя каталога, который надо добавить
// в PATH. pip кладёт консольные скрипты в Scripts (Scripts\ на Windows), и его
// обычно там нет: поэтому «pip install X» для уже установленного пакета не
// помогает ничего — это был ложный совет.
func pathAdvice(rep Report, p Plugin) string {
	var b strings.Builder
	b.WriteString("пакет уже установлен, но его скрипта нет в PATH — установка не поможет")
	if len(p.DonorScripts) > 0 {
		b.WriteString("; скрипт пакета: " + strings.Join(p.DonorScripts, ", "))
	}
	if rep.Python.ScriptsDir != "" {
		if rep.Python.ScriptsDirInPATH {
			b.WriteString("; каталог " + rep.Python.ScriptsDir + " в PATH есть — проверь имя скрипта")
		} else {
			b.WriteString("; добавь в PATH: " + rep.Python.ScriptsDir)
		}
	}
	return b.String()
}
