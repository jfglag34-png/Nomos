package api

// Потеря данных при сохранении из GUI: round-trip каждого пресета реестра
// через настоящий путь редактора (parse → editor-doc → serialize → parse) и
// сравнение ПО СМЫСЛУ, а не по строкам.
//
// Почему сравнение по смыслу. Редактор пишет свой YAML заново: порядок ключей
// меняется (network уезжает за steps — это outFile, а не потеря), длительности
// печатаются как Duration.String() (120s → 2m0s — та же длительность), а
// pos проставляется каждому шагу. Сравнение байтов поймало бы нормализацию и
// пропустило бы потерю. Сравниваются значения полей схемы ядра.
//
// Что здесь было. Находка: examples/email_triage_chain.yaml «переписали
// редактором» — пропал блок foreach, network: allow уехал вглубь шагов,
// on_error поменялся со skip на stop. Проверено на живом GUI: на текущем коде
// ничего из этого не повторяется — все 22 пресета переживают round-trip без
// потери полей. Историческая сборка (8089e91, 2026-09-25) теряла только
// явный network: allow, а сборки до b66fdbd (2026-09-06) вовсе отказывались
// сохранять файл с pipeline.foreach. Тест нужен, чтобы это свойство не
// разъехалось снова: поле, которого нет ни в editorDoc, ни в outFile, выпало
// бы из обоих концов молча.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/registry"
)

// presetPaths — все пресеты реестра: тест обязан знать не «один удобный
// пример», а каждый зарегистрированный сценарий. Реестр лежит в корне
// репозитория, а тест запускается из internal/api.
func presetPaths(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	handle, err := registry.Load(root)
	if err != nil {
		t.Fatalf("реестр не читается: %v", err)
	}
	if len(handle.Registry.Presets) == 0 {
		t.Fatal("в реестре нет ни одного пресета")
	}
	out := make([]string, 0, len(handle.Registry.Presets))
	for name, entry := range handle.Registry.Presets {
		if entry.Path == "" {
			t.Fatalf("пресет %s: путь не задан", name)
		}
		out = append(out, filepath.Join(root, filepath.FromSlash(entry.Path)))
	}
	sort.Strings(out)
	return out
}

// effectiveOnError — пустое поле и «stop» это ОДНО И ТО ЖЕ: раннер разбирает
// on_error через switch, где и "", и "stop" попадают в default (элемент
// останавливается), а валидатор принимает и "", и "stop". Редактор дописывает
// «stop» явно — это нормализация. А вот skip → stop меняла бы смысл (плохой
// элемент ронял бы весь ран вместо своего), и такие расхождения тест ловит.
func effectiveOnError(v string) string {
	if v == "" {
		return "stop"
	}
	return v
}

// pipelineShape — значение всех полей схемы ядра в виде, где сравнение
// осмысленно: длительности приведены к наносекундам, on_error — к эффективному
// значению, а pos отброшен (его редактор пересчитывает, ядро его игнорирует).
func pipelineShape(pf *pipeline.PipelineFile) map[string]interface{} {
	steps := make([]interface{}, 0, len(pf.Pipeline.Steps))
	for _, st := range pf.Pipeline.Steps {
		form := make([]interface{}, 0, len(st.Form))
		for _, f := range st.Form {
			form = append(form, map[string]interface{}{
				"field": f.Field, "editable": f.Editable, "type": f.Type, "format": f.Format,
			})
		}
		step := map[string]interface{}{
			"id": st.ID, "plugin": st.Plugin, "on_error": effectiveOnError(st.OnError),
			"timeout_ns": int64(st.Timeout.Duration), "bind": st.Bind,
			"after_foreach": st.AfterForeach, "foreach": st.Foreach,
			"foreach_item": st.ForeachItem, "parallel_group": st.ParallelGroup,
			"loop": st.Loop, "loop_condition": st.LoopCondition,
			"max_iterations": st.MaxIterations,
			"form":           form, "actions": st.Actions,
			"on_reject": st.OnReject, "approval": st.Approval,
			"when": map[string]interface{}{
				"set": st.When.IsSet(), "path": st.When.Path, "op": st.When.Op, "value": st.When.Value,
			},
			"retry": nil,
		}
		if st.Retry != nil {
			step["retry"] = map[string]interface{}{
				"attempts": st.Retry.Attempts,
				"delay_ns": int64(st.Retry.Delay.Duration),
				"backoff":  st.Retry.Backoff,
			}
		}
		steps = append(steps, step)
	}
	input := map[string]interface{}{}
	for k, v := range pf.Pipeline.Input {
		input[k] = v
	}
	return map[string]interface{}{
		"name": pf.Pipeline.Name, "input": input, "steps": steps,
		"foreach": pf.Pipeline.Foreach, "foreach_item": pf.Pipeline.ForeachItem,
		"item_type": pf.Pipeline.ItemType, "item_format": pf.Pipeline.ItemFormat,
		"secrets": pf.Pipeline.Secrets, "network": pf.Pipeline.Network,
		"gates": pf.Pipeline.Gates,
	}
}

// diffShape — имена полей, значения которых разошлись. Путь строится с
// индексами шага: «шаг 2 (triage).on_error».
func diffShape(a, b map[string]interface{}, prefix string) []string {
	var out []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		av, aok := a[k]
		bv, bok := b[k]
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if !aok {
			out = append(out, path+": появилось в копии")
			continue
		}
		if !bok {
			out = append(out, path+": ПОТЕРЯНО ("+briefShape(av)+")")
			continue
		}
		am, aIsMap := av.(map[string]interface{})
		bm, bIsMap := bv.(map[string]interface{})
		if aIsMap && bIsMap {
			out = append(out, diffShape(am, bm, path)...)
			continue
		}
		as, aIsList := av.([]interface{})
		bs, bIsList := bv.([]interface{})
		if aIsList && bIsList {
			if len(as) != len(bs) {
				out = append(out, path+": длина "+itoa(len(as))+" → "+itoa(len(bs)))
				continue
			}
			for i := range as {
				ap, bp := as[i], bs[i]
				stepPath := path + "[" + itoa(i) + "]"
				if am2, ok := ap.(map[string]interface{}); ok {
					if bm2, ok2 := bp.(map[string]interface{}); ok2 {
						label := stepPath
						if id, has := am2["id"].(string); has {
							label = path + " (шаг " + id + ")"
						}
						out = append(out, diffShape(am2, bm2, label)...)
						continue
					}
				}
				if !reflect.DeepEqual(ap, bp) {
					out = append(out, stepPath+": "+briefShape(ap)+" → "+briefShape(bp))
				}
			}
			continue
		}
		if !reflect.DeepEqual(av, bv) {
			out = append(out, path+": "+briefShape(av)+" → "+briefShape(bv))
		}
	}
	return out
}

func briefShape(v interface{}) string {
	raw, _ := json.Marshal(v)
	text := string(raw)
	if len(text) > 70 {
		text = text[:70] + "…"
	}
	return text
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

// TestEditorRoundTripPresetsNoFieldLoss — главный страж: каждый пресет
// реестра проходит путь редактора и не теряет ни одного поля схемы.
func TestEditorRoundTripPresetsNoFieldLoss(t *testing.T) {
	ts, _ := gateTestServer(t)
	paths := presetPaths(t)
	t.Logf("пресетов в реестре: %d", len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("пресет не читается: %v", err)
			}
			before, err := pipeline.LoadPipelineFileFromBytes(raw)
			if err != nil {
				t.Fatalf("ядро не читает пресет: %v", err)
			}

			// 1. parse → editor-doc (именно этот JSON получает браузер)
			code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
			if code != 200 {
				t.Fatalf("parse: code=%d body=%v", code, doc)
			}
			unsupported, _ := doc["unsupported"].([]interface{})
			if len(unsupported) != 0 {
				t.Fatalf("пресет помечен неуправляемым: %v — редактор откажется его сохранять", unsupported)
			}

			// 2. doc → serialize → YAML (что уходит в PUT /api/pipelines)
			code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", mustJSON(t, doc))
			if code != 200 {
				t.Fatalf("serialize: code=%d body=%v", code, out)
			}
			yamlText, _ := out["yaml"].(string)
			if yamlText == "" {
				t.Fatalf("serialize не отдал yaml: %v", out)
			}

			// 3. YAML → ядро: так же, как это сделает следующий запуск
			after, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
			if err != nil {
				t.Fatalf("сгенерированный YAML не читается ядром: %v\n%s", err, yamlText)
			}

			if diff := diffShape(pipelineShape(before), pipelineShape(after), ""); len(diff) != 0 {
				t.Fatalf("поля не пережили round-trip:\n  %s\n--- yaml редактора ---\n%s",
					joinLines(diff), yamlText)
			}

			// 4. валидация не должна стать хуже. Сравниваются не тексты, а
			// КОДЫ ошибок: почему конкретная ошибка есть — зависит от того,
			// установлены ли плагины (в тесте каталог plugins пуст), а вот
			// «редактор добавил ошибку, которой не было» — свойство файла.
			// Проверка на живом GUI с настоящими манифестами: ok=22/22.
			wasCodes := errorCodes(t, ts.URL, raw)
			nowCodes := errorCodes(t, ts.URL, []byte(yamlText))
			for _, code := range nowCodes {
				if !contains(wasCodes, code) {
					t.Errorf("редактор добавил ошибку валидации, которой не было: %s\n%s", code, yamlText)
				}
			}
		})
	}
}

// TestEditorRoundTripKeepsNetworkAndSecrets — те же поля отдельно, с
// утверждениями, которые читаются как обвинение: явный network: allow и
// объявленный ключ обязаны пережить round-trip. Именно их теряла сборка
// 8089e91 («allow считался дефолтом и не писался в YAML»), и ровно их не
// переносил сборщик цепочки в браузере.
func TestEditorRoundTripKeepsNetworkAndSecrets(t *testing.T) {
	ts, _ := gateTestServer(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "llm_same_provider.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("parse: code=%d body=%v", code, doc)
	}
	if doc["network"] != "allow" {
		t.Fatalf("parse потерял network: %v", doc["network"])
	}
	if _, ok := doc["secrets"].([]interface{}); !ok {
		t.Fatalf("parse потерял secrets: %v", doc["secrets"])
	}
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", mustJSON(t, doc))
	if code != 200 {
		t.Fatalf("serialize: code=%d body=%v", code, out)
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(out["yaml"].(string)))
	if err != nil {
		t.Fatalf("сгенерированный YAML не читается: %v", err)
	}
	if pf.Pipeline.Network != "allow" {
		t.Errorf("network: allow не пережил round-trip — получено %q (пустое поле теперь означает deny, и ран откажет)", pf.Pipeline.Network)
	}
	if len(pf.Pipeline.Secrets) == 0 {
		t.Errorf("pipeline.secrets не пережил round-trip: %v", pf.Pipeline.Secrets)
	}
}

// errorCodes — коды ошибок валидации для YAML. Сортируются: порядок замечаний
// зависит от обхода карты и менялся бы сам по себе.
func errorCodes(t *testing.T, base string, yamlText []byte) []string {
	t.Helper()
	resp, err := http.Post(base+"/api/validate/pipeline", "application/yaml", bytes.NewReader(yamlText))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Issues []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("validate: ответ не разобран: %v", err)
	}
	var codes []string
	for _, issue := range out.Issues {
		if issue.Severity == "error" || issue.Code == "" {
			if issue.Severity != "warning" {
				codes = append(codes, issue.Code)
			}
		}
	}
	sort.Strings(codes)
	return codes
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func joinLines(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}

var _ = http.StatusOK
