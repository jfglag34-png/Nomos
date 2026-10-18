package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

// ConformanceCheck — одна проверка ядра/протокола.
type ConformanceCheck struct {
	Name    string `json:"name"`
	Pass    bool   `json:"pass"`
	Details string `json:"details,omitempty"`
}

// ConformanceReport — машиночитаемый отчёт для CI (+ бейдж).
type ConformanceReport struct {
	OK     bool               `json:"ok"`
	Checks []ConformanceCheck `json:"checks"`
}

func confPass(name string) ConformanceCheck { return ConformanceCheck{Name: name, Pass: true} }
func confFail(name, details string) ConformanceCheck {
	return ConformanceCheck{Name: name, Pass: false, Details: details}
}

func defaultConformanceFixturesDir(fixturesDir string) string {
	if strings.TrimSpace(fixturesDir) != "" {
		return fixturesDir
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "conformance", "fixtures", "v0.2"),
			filepath.Join(cwd, "internal", "core", "testdata", "plugins"),
		)
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "testdata", "plugins"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "conformance", "fixtures", "v0.2"),
			filepath.Join(dir, "internal", "core", "testdata", "plugins"),
		)
	}
	for _, candidate := range candidates {
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate
		}
	}
	return filepath.Join("internal", "core", "testdata", "plugins")
}

// RunConformance — батарея ядра по фикстурам fixturesDir
// (по умолчанию conformance/fixtures/v0.2, затем development testdata):
// строгая загрузка всех fixture manifests, handshake, big_stdout 17MB→protocol_violation,
// big_stderr 2MB→ok, cancel (sleeper+отмена → cancelled, не timeout), error_codes (golden Issue).
func RunConformance(fixturesDir string) ConformanceReport {
	fixturesDir = defaultConformanceFixturesDir(fixturesDir)
	var checks []ConformanceCheck

	// Доверие фикстурам: конформность гоняет плагины ИЗ КАТАЛОГА, который
	// оператор назвал (или который сама функция выбрала из каталога ядра). Это
	// прямое указание человека, а не самообъявление плагина, — по сути тем же
	// основанием, что и `wedra plugin test <dir>`. Без этого батарея падала бы
	// на отказе по политике и не проверяла бы ничего.
	fixtureCtx := context.Background()
	if list, err := plugin.AllowListFromDirs(fixturesDir); err == nil {
		fixtureCtx = plugin.WithTrustPolicy(fixtureCtx, plugin.TrustPolicy{Trusted: list})
	}

	load := func(name string) *Manifest {
		eng := NewEngine()
		m, err := eng.LoadManifest(filepath.Join(fixturesDir, name))
		if err != nil {
			return nil
		}
		return m
	}

	entries, readErr := os.ReadDir(fixturesDir)
	loadedFixtures := 0
	var fixtureErrors []string
	if readErr != nil {
		fixtureErrors = append(fixtureErrors, readErr.Error())
	} else {
		manifestEngine := NewEngine()
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(fixturesDir, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, "plugin.yaml")); err != nil {
				continue
			}
			if _, err := manifestEngine.LoadManifest(dir); err != nil {
				fixtureErrors = append(fixtureErrors, entry.Name()+": "+err.Error())
				continue
			}
			loadedFixtures++
		}
	}
	// Имя с уточнением: этот чек проверяет ЗАГРУЗКУ манифестов, а не поведение.
	// Раньше он назывался fixture_manifests_17 и наравне с поведенческими
	// чеками выглядел как «17 фикстур пройдено» — хотя исполнялись четыре.
	if len(fixtureErrors) == 0 && loadedFixtures > 0 {
		checks = append(checks, ConformanceCheck{
			Name:    "fixture_manifests_loaded_" + strconv.Itoa(loadedFixtures),
			Pass:    true,
			Details: "манифесты прочитаны и провалидированы; исполнение — в чеках ниже и в fixture_coverage",
		})
	} else {
		details := strings.Join(fixtureErrors, "; ")
		if details == "" {
			// Каталог читается, но фикстур в нём нет: без объяснения отчёт
			// показывал бы провал с пустой причиной.
			details = "в каталоге " + fixturesDir + " нет ни одной фикстуры (plugin.yaml)"
		}
		checks = append(checks, confFail("fixture_manifests_loaded", details))
	}

	// Исполнять нечего — и продолжать нельзя: каждый поведенческий чек ждёт
	// результат запуска фикстуры, а результата не будет. До этой проверки отчёт
	// умирал паникой (nil dereference) на первом же чеке; воспроизводилось на
	// `plugin test <плагин> --conformance`, где позиционный аргумент — это
	// каталог фикстур, а не плагин. Одна причина внятнее дюжины одинаковых
	// провалов «<фикстура> не загрузился».
	if readErr != nil || loadedFixtures == 0 {
		reason := "в каталоге фикстур " + fixturesDir + " нет ни одной фикстуры (plugin.yaml)"
		if readErr != nil {
			reason = fmt.Sprintf("каталог фикстур %q не читается: %v", fixturesDir, readErr)
		}
		checks = append(checks, confFail("fixtures_dir",
			reason+"; в режиме --conformance позиционный аргумент и --fixtures задают каталог фикстур, а не сам плагин"))
		return ConformanceReport{OK: false, Checks: checks}
	}

	// executed — фикстуры, которые батарея действительно запускала. Отчёт
	// обязан отличать «манифест прочитался» от «плагин исполнялся»: раньше
	// единственный чек назывался fixture_manifests_17 и читался как полное
	// соответствие, хотя из 17 фикстур запускались четыре.
	executed := map[string]bool{}

	// fixtureRun — прогнать фикстуру и отдать её результат проверке. Помощник
	// для всех поведенческих проверок ниже: раньше они повторяли одну и ту же
	// защиту от nil-манифеста, и лишние фикстуры просто не попадали в батарею.
	fixtureRun := func(name, input string, timeout time.Duration) (*pipeline.Manifest, *plugin.ExecResult, ConformanceCheck) {
		m := load(name)
		if m == nil {
			// Имя чека обязательно: вызывающие отличают «провал» от «проверки
			// не было» по bad.Name != "". Пустое имя означало «проверки нет»,
			// а вместе с ним наружу уходил nil-результат — и вызов падал на
			// !res.Platform. Отсутствующая фикстура — это провал, и он должен
			// быть назван, а не проглочен.
			return nil, nil, confFail(name+"_fixture", name+" не загрузился")
		}
		executed[name] = true
		return m, plugin.ExecWithEnvCtx(fixtureCtx, m, []byte(input), timeout, []string{"WEDRA_NETWORK=deny"}), ConformanceCheck{}
	}

	// 1. handshake: echo_ok
	executed["echo_ok"] = true
	if m := load("echo_ok"); m == nil {
		checks = append(checks, confFail("handshake", "echo_ok не загрузился"))
	} else {
		res := plugin.ExecWithEnvCtx(fixtureCtx, m, []byte("{}"), 10*time.Second, nil)
		if res.OK() {
			checks = append(checks, confPass("handshake"))
		} else {
			checks = append(checks, confFail("handshake", "code="+res.ErrCode+" msg="+res.ErrMsg))
		}
	}

	// Аварийное завершение: exit >= 2 → platform:* и ран останавливается
	// (PROTOCOL §3). Фикстуры раньше грузились, но не исполнялись: кейс
	// «плагин упал с exit 2» в батарее не проверялся вообще.
	if _, res, bad := fixtureRun("crasher", "{}", 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if !res.Platform || res.ExitCode < 2 {
		checks = append(checks, confFail("crash_exit2", fmt.Sprintf("ожидался platform-отказ при exit>=2, получено platform=%v exit=%d code=%q", res.Platform, res.ExitCode, res.ErrCode)))
	} else {
		checks = append(checks, confPass("crash_exit2"))
	}

	// Мусор в stdout вместо JSON → protocol_violation, а не «доменная ошибка»:
	// ядро не может ни разобрать ответ, ни отличить его от аварии.
	if _, res, bad := fixtureRun("bad_proto", "{}", 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if !res.Platform || res.ErrCode != "protocol_violation" {
		checks = append(checks, confFail("bad_proto", "want platform/protocol_violation, got platform="+strconv.FormatBool(res.Platform)+" code="+res.ErrCode))
	} else {
		checks = append(checks, confPass("bad_proto"))
	}

	// Незадекларированное поле отбрасывается, а не падает рантайм.
	if m, res, bad := fixtureRun("leaker", "{}", 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else {
		clean, dropped, err := plugin.EnforceOutput(m, res.Output)
		switch {
		case err != nil:
			checks = append(checks, confFail("undeclared_output", err.Error()))
		case len(dropped) == 0:
			checks = append(checks, confFail("undeclared_output", "незадекларированное поле не отброшено: "+fmt.Sprint(clean)))
		case clean["value"] != "x":
			checks = append(checks, confFail("undeclared_output", "объявленное поле потеряно: "+fmt.Sprint(clean)))
		default:
			checks = append(checks, confPass("undeclared_output"))
		}
	}

	// Дрейф типа: манифест обещает string, плагин отдал number.
	if m, res, bad := fixtureRun("type_drifter", "{}", 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if _, _, err := plugin.EnforceOutput(m, res.Output); err == nil {
		checks = append(checks, confFail("type_drift", "число прошло проверку string — контракт типов не работает"))
	} else {
		checks = append(checks, confPass("type_drift"))
	}

	// Обязательное поле не вернули → нарушение контракта.
	if m, res, bad := fixtureRun("contract_breaker", "{}", 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if _, _, err := plugin.EnforceOutput(m, res.Output); err == nil {
		checks = append(checks, confFail("required_output", "пустой вывод прошёл проверку обязательного поля"))
	} else {
		checks = append(checks, confPass("required_output"))
	}

	// Число на входе и выходе: type: number не должен ломаться на int/float.
	if m, res, bad := fixtureRun("num_only", `{"n":7}`, 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if _, _, err := plugin.EnforceOutput(m, res.Output); err != nil {
		checks = append(checks, confFail("number_roundtrip", err.Error()))
	} else {
		checks = append(checks, confPass("number_roundtrip"))
	}

	// optional-вход отсутствует: плагин обязан получить «просто без него», а не
	// ошибку сборки входа. Проверка на подключённом шаге — в core/plugintest.
	if m, res, bad := fixtureRun("consumer_opt", "{}", 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if _, _, err := plugin.EnforceOutput(m, res.Output); err != nil {
		checks = append(checks, confFail("optional_input", err.Error()))
	} else if res.Output["got"] != "<none>" {
		checks = append(checks, confFail("optional_input", "плагин не увидел отсутствие optional-входа: "+fmt.Sprint(res.Output)))
	} else {
		checks = append(checks, confPass("optional_input"))
	}

	// Сеть: пустой WEDRA_NETWORK плагину не достаётся, вместо него deny.
	// Фикстура net_probe отдаёт значение переменной — контракт «declare-now».
	if m, res, bad := fixtureRun("net_probe", `{"value":"x"}`, 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if _, _, err := plugin.EnforceOutput(m, res.Output); err != nil {
		checks = append(checks, confFail("network_env", err.Error()))
	} else if res.Output["value"] != "deny" {
		checks = append(checks, confFail("network_env", "WEDRA_NETWORK не дошёл до плагина: "+fmt.Sprint(res.Output)))
	} else {
		checks = append(checks, confPass("network_env"))
	}

	// file_ref: относительный путь не найден от рабочей директории плагина —
	// обязан быть warning, а не тишиной (иначе плагин прочитает не то).
	if m, res, bad := fixtureRun("file_ref_echo", `{"path":"missing.txt"}`, 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else {
		if _, _, err := plugin.EnforceOutput(m, res.Output); err != nil {
			checks = append(checks, confFail("file_ref_warning", err.Error()))
		} else if warns := plugin.FileRefWarnings(m, map[string]interface{}{"path": "missing.txt"}); len(warns) == 0 {
			checks = append(checks, confFail("file_ref_warning", "относительный file_ref без warning: subprocess стартует в каталоге плагина"))
		} else {
			checks = append(checks, confPass("file_ref_warning"))
		}
	}

	// Доменная ошибка (exit 1) — не платформенная: ран может её пережить по
	// on_error, и ядро обязано отличать её от аварии exit>=2.
	if _, res, bad := fixtureRun("failer", `{"value":"bad"}`, 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if res.Platform || res.ErrCode != "bad_value" {
		checks = append(checks, confFail("domain_error", "ожидался код bad_value без platform, получено platform="+strconv.FormatBool(res.Platform)+" code="+res.ErrCode))
	} else {
		checks = append(checks, confPass("domain_error"))
	}

	// retryable=true обязан доехать до ядра: без него retry шага не срабатывает
	// и «мигающий» плагин падает вместо повтора.
	if _, res, bad := fixtureRun("failer", `{"value":"flaky"}`, 10*time.Second); bad.Name != "" {
		checks = append(checks, bad)
	} else if !res.Retryable {
		checks = append(checks, confFail("retryable_flag", "retryable=true не дошёл до ExecResult"))
	} else {
		checks = append(checks, confPass("retryable_flag"))
	}

	// 2. big_stdout 17MB → protocol_violation
	executed["chatter"] = true
	if m := load("chatter"); m == nil {
		checks = append(checks, confFail("big_stdout", "chatter не загрузился"))
	} else {
		res := plugin.ExecWithEnvCtx(fixtureCtx, m, []byte("{}"), 30*time.Second, nil)
		if res.Platform && res.ErrCode == "protocol_violation" {
			checks = append(checks, confPass("big_stdout"))
		} else {
			checks = append(checks, confFail("big_stdout", "want protocol_violation, got "+res.ErrCode))
		}
	}

	// 3. big_stderr 2MB → ok
	executed["big_stderr"] = true
	if m := load("big_stderr"); m == nil {
		checks = append(checks, confFail("big_stderr", "big_stderr не загрузился"))
	} else {
		res := plugin.ExecWithEnvCtx(fixtureCtx, m, []byte("{}"), 30*time.Second, nil)
		if res.OK() {
			checks = append(checks, confPass("big_stderr"))
		} else {
			checks = append(checks, confFail("big_stderr", "want ok, got "+res.ErrCode))
		}
	}

	// 4. cancel: sleeper + отмена через 300ms → cancelled, не timeout
	executed["sleeper"] = true
	if m := load("sleeper"); m == nil {
		checks = append(checks, confFail("cancel", "sleeper не загрузился"))
	} else {
		// Отмена наследует политику доверия фикстур: context.WithCancel поверх
		// context.Background() её бы потерял, и плагин ушёл бы в песочницу.
		ctx, cancel := context.WithCancel(fixtureCtx)
		done := make(chan *plugin.ExecResult, 1)
		go func() { done <- plugin.ExecWithEnvCtx(ctx, m, []byte("{}"), 30*time.Second, nil) }()
		time.Sleep(300 * time.Millisecond)
		cancel()
		select {
		case res := <-done:
			if res.Cancelled && res.ErrCode == "cancelled" {
				checks = append(checks, confPass("cancel"))
			} else {
				checks = append(checks, confFail("cancel", "want cancelled, got "+res.ErrCode))
			}
		case <-time.After(15 * time.Second):
			checks = append(checks, confFail("cancel", "не завершился за 15с после cancel"))
		}
	}

	// 5. error_codes: golden Issue-коды (контракт для агентов)
	eng := NewEngine()
	mkBad := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "bad",
			Input: map[string]interface{}{},
			Steps: []pipeline.Step{{ID: "s", Plugin: "fake/nope", Bind: map[string]string{"x": "input.nope"}}},
		},
	}
	issues := pipeline.ValidateIssues(mkBad, eng)
	found := false
	for _, is := range issues {
		if is.Code == pipeline.E_PLUGIN_LOAD || is.Code == pipeline.E_PORT_SOURCE {
			found = true
			break
		}
	}
	if found {
		checks = append(checks, confPass("error_codes"))
	} else {
		// Коды собираются не для красоты: без них в отчёте видно только
		// «ожидаемого кода нет», и непонятно, что пришло вместо него. Раньше
		// срез заполнялся и выбрасывался — ровно тот случай, когда проверка
		// знает ответ и молчит.
		codes := make([]string, 0, len(issues))
		for _, is := range issues {
			codes = append(codes, is.Code)
		}
		msg := "нет E_PLUGIN_LOAD/E_PORT_SOURCE"
		if len(codes) == 0 {
			msg += "; валидатор не выдал ни одного кода"
		} else {
			msg += "; получено: " + strings.Join(codes, ", ")
		}
		checks = append(checks, confFail("error_codes", msg))
	}

	// Покрытие фикстур — отдельная проверка, потому что отчёт обязан отличать
	// «манифест прочитался» от «плагин исполнялся». Чек fixture_manifests_17
	// читался как «всё соответствует», хотя запускались 4 фикстуры из 17.
	//
	// Новая фикстура обязана быть либо исполнена, либо названа в
	// fixtureNotExecuted с причиной: иначе батарея снова начнёт считать
	// неисполненное покрытым, а это ровно тот дефект, который здесь чинится.
	fixtureNotExecuted := map[string]string{
		"net_demo":    "объявляет сеть host:port; проверяется политикой (net_probe + execution), сам плагин сеть не трогает",
		"retry_flaky": "состояние в файле _counter рядом с main.py: запуск в батарее писал бы в дерево фикстур (покрыт в core/plugintest)",
		"spawner":     "рожает дочерний процесс и требует SPID_FILE; проверяется process-group kill в internal/plugin (платформенно-зависимо)",
	}
	var missing, skipped []string
	for _, entry := range entries {
		if !entry.IsDir() || executed[entry.Name()] {
			continue
		}
		if reason, ok := fixtureNotExecuted[entry.Name()]; ok {
			skipped = append(skipped, entry.Name()+" — "+reason)
			continue
		}
		missing = append(missing, entry.Name())
	}
	coverage := fmt.Sprintf("исполнено %d из %d", len(executed), loadedFixtures)
	switch {
	case len(missing) > 0:
		checks = append(checks, confFail("fixture_coverage",
			coverage+"; не исполнены и не объяснены: "+strings.Join(missing, ", ")))
	case len(skipped) > 0:
		checks = append(checks, ConformanceCheck{Name: "fixture_coverage", Pass: true,
			Details: coverage + "; пропущены осознанно: " + strings.Join(skipped, "; ")})
	default:
		checks = append(checks, ConformanceCheck{Name: "fixture_coverage", Pass: true, Details: coverage})
	}

	ok := true
	for _, c := range checks {
		if !c.Pass {
			ok = false
			break
		}
	}
	return ConformanceReport{OK: ok, Checks: checks}
}
