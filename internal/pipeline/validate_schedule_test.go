package pipeline

import (
	"testing"
	"time"
)

// Валидация блока schedule до этих правок проверяла cron, overlap и human_gate,
// но не проверяла ничего, что относится к alerts и gate_timeout. Отсюда был
// конфиг, который проходил гейт зелёным, а демон по нему либо молчал, либо
// пытался отправить мусор в сеть.

// schedStub — заглушка движка. Engine это интерфейс LoadManifest, а валидация
// шагов дёргает его ради сетевых деклараций. Про шаги нам проверять нечего,
// тесты про schedule, поэтому используется чужая заглушка пакета: своего
// stubEngine в нём уже достаточно.
type schedStub struct{}

func (*schedStub) LoadManifest(ref string) (*Manifest, error) {
	return &Manifest{ID: ref}, nil
}

// pfWithSchedule — минимальный валидный пайплайн с заданным расписанием.
func pfWithSchedule(s *Schedule) *PipelineFile {
	return &PipelineFile{
		FormatVersion: "0.2",
		Pipeline: Pipeline{
			Name:     "sched_probe",
			Network:  "allow",
			Schedule: s,
			Input:    map[string]interface{}{"domain": "example.com"},
			Steps:    []Step{{ID: "s1", Plugin: "core/stub", Bind: map[string]string{"domain": "input.domain"}}},
		},
	}
}

// hasCode — есть ли среди issues код. Свой помощник, чтобы тесты не зависели от
// порядка issues и от их количества.
func hasCode(issues []Issue, code string) bool {
	for _, is := range issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

// TestValidateScheduleAlertsWebhook — вебхук обязан быть настоящим адресом.
//
// Схемы file:// и ftp:// запрещены осознанно: доставить туда нечем, а такой
// конфиг раньше проходил насквозь и выглядел как «алерты настроены».
func TestValidateScheduleAlertsWebhook(t *testing.T) {
	cases := []struct {
		name    string
		webhook string
		wantErr bool
	}{
		{"https", "https://example.com/hook", false},
		{"http", "http://example.com/hook", false},
		{"не урл вообще", "не урл вообще", true},
		{"file://", "file:///etc/passwd", true},
		{"ftp://", "ftp://example.com/hook", true},
		{"без узла", "https://", true},
		{"пусто", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := pfWithSchedule(&Schedule{
				Cron:   "0 9 * * *",
				Alerts: ScheduleAlerts{Webhook: tc.webhook},
			})
			issues := ValidateIssues(pf, &schedStub{})
			if got := hasCode(issues, E_SCHEDULE_ALERTS); got != tc.wantErr {
				t.Errorf("E_SCHEDULE_ALERTS = %v, ожидали %v; issues=%v", got, tc.wantErr, issues)
			}
		})
	}
}

// TestValidateScheduleAlertsNotifierShape — форма имени нотификатора.
//
// Существование плагина здесь НЕ проверяется сознательно: валидатор не видит
// реестр, и community-плагины он для шагов тоже не проверяет. Ввести здесь
// реестр означало бы запрет валидировать пайплайн до установки его плагинов.
// Ловится форма — пробелы и значение из одних пробелов, то есть опечатки.
func TestValidateScheduleAlertsNotifierShape(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"нормальное имя", "telegram_notifier", false},
		{"подчёркивание и точка", "my.notifier_v2", false},
		{"пробелы внутри", "telegram notifier", true},
		{"только пробелы", "   ", true},
		{"пусто", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := pfWithSchedule(&Schedule{
				Cron:   "0 9 * * *",
				Alerts: ScheduleAlerts{OnWaiting: tc.val},
			})
			issues := ValidateIssues(pf, &schedStub{})
			if got := hasCode(issues, E_SCHEDULE_ALERTS); got != tc.wantErr {
				t.Errorf("E_SCHEDULE_ALERTS = %v, ожидали %v; issues=%v", got, tc.wantErr, issues)
			}
		})
	}
}

// TestValidateScheduleOnTimeout — on_timeout допускает ровно два значения.
// Любое другое, включая опечатку, трактуется как ошибка, а не как «ну ладно».
func TestValidateScheduleOnTimeout(t *testing.T) {
	cases := []struct {
		val     string
		wantErr bool
	}{
		{"abort", false},
		{"auto_approve", false},
		{"auto-accept", true},
		{"да", true},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("on_timeout="+tc.val, func(t *testing.T) {
			pf := pfWithSchedule(&Schedule{Cron: "0 9 * * *", OnTimeout: tc.val})
			issues := ValidateIssues(pf, &schedStub{})
			if got := hasCode(issues, E_SCHEDULE_ALERTS); got != tc.wantErr {
				t.Errorf("on_timeout=%q: E_SCHEDULE_ALERTS = %v, ожидали %v", tc.val, got, tc.wantErr)
			}
		})
	}
}

// TestValidateScheduleGateTimeoutSign — отрицательный потолок бессмысленен:
// «ждать -5 минут» это то же, что не задать потолок, но выглядит как будто он
// настроен.
func TestValidateScheduleGateTimeoutSign(t *testing.T) {
	cases := []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{"отрицательный", -5 * time.Minute, true},
		{"нулевой — не задан", 0, false},
		{"положительный", 30 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := pfWithSchedule(&Schedule{
				Cron:        "0 9 * * *",
				GateTimeout: Duration{Duration: tc.d},
			})
			issues := ValidateIssues(pf, &schedStub{})
			if got := hasCode(issues, E_SCHEDULE_ALERTS); got != tc.wantErr {
				t.Errorf("gate_timeout=%v: E_SCHEDULE_ALERTS = %v, ожидали %v", tc.d, got, tc.wantErr)
			}
		})
	}
}

// TestValidateTimezoneReallyChecks — регрессия на найденный дефект.
//
// Раньше validateTimezone начиналась с `if strings.Contains(tz, "/") { return
// nil }`. Любая IANA-зона содержит слеш, поэтому проверка пропускала ровно те
// значения, ради которых была написана, и до time.LoadLocation доходила почти
// никогда: «Плутон/Город» проходил наравне с Europe/Minsk.
//
// Обход был обходом отсутствия базы зон: time/tzdata не импортировался, зоны
// читались из GOROOT, которого у пользователя готового бинарника нет. База
// теперь встроена, и проверка стала настоящей. Тест фиксирует факт
// срабатывания, а не формулировку сообщения.
func TestValidateTimezoneReallyChecks(t *testing.T) {
	cases := []struct {
		tz      string
		wantErr bool
	}{
		{"Europe/Minsk", false},
		{"Asia/Tokyo", false},
		{"UTC", false},
		{"Local", false},
		{"Плутон/Город", true},
		{"Europe/Minskx", true},
		{"MSK", true},
		{"Not/AZone", true},
	}
	for _, tc := range cases {
		t.Run(tc.tz, func(t *testing.T) {
			if err := validateTimezone(tc.tz); (err != nil) != tc.wantErr {
				t.Errorf("validateTimezone(%q): ошибка=%v, ожидали hasErr=%v", tc.tz, err, tc.wantErr)
			}
		})
	}
}

// TestValidScheduleStillPasses — страховка от регрессии наоборот: нормальный
// конфиг со всем новым обязан остаться зелёным.
func TestValidScheduleStillPasses(t *testing.T) {
	pf := pfWithSchedule(&Schedule{
		Cron:        "0 9 * * *",
		Timezone:    "Europe/Minsk",
		Overlap:     "skip",
		HumanGate:   "wait",
		GateTimeout: Duration{Duration: 30 * time.Minute},
		OnTimeout:   "abort",
		Alerts:      ScheduleAlerts{OnWaiting: "telegram_notifier", Webhook: "https://example.com/hook"},
	})
	for _, is := range ValidateIssues(pf, &schedStub{}) {
		switch is.Code {
		case E_SCHEDULE_ALERTS, E_SCHEDULE_TIMEZONE, E_SCHEDULE_CRON, E_SCHEDULE_OVERLAP, E_SCHEDULE_HUMAN_GATE:
			t.Errorf("валидный конфиг получил ошибку расписания: %+v", is)
		}
	}
}
