package cron

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// Претензия «тестов на алерты: 0» из ревью была верной: fireAlert состоял из
// двух `return nil`, и «проверить» в нём было нечего. Теперь есть что, и
// проверять есть что — в первую очередь молчание. Молчащее оповещение хуже
// отсутствующего: человек прописал alerts и ждал, что придёт.

// TestFireAlertDeliversWebhook — основной путь: подписан вебхук, значит событие
// доходит. Проверяется и метод, и тело, и заголовок: получатель на той стороне
// разбирает именно это.
func TestFireAlertDeliversWebhook(t *testing.T) {
	var gotMethod, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sched := &pipeline.Schedule{Alerts: pipeline.ScheduleAlerts{
		OnWaiting: "1",
		Webhook:   srv.URL,
	}}
	if err := fireAlert(sched, "on_waiting", "examples/p.yaml", "human gate waiting"); err != nil {
		t.Fatalf("fireAlert вернул ошибку при рабочем вебхуке: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("метод = %q, ожидался POST", gotMethod)
	}
	if !strings.Contains(gotType, "application/json") {
		t.Errorf("Content-Type = %q, ожидался application/json", gotType)
	}
	var p alertPayload
	if err := json.Unmarshal([]byte(gotBody), &p); err != nil {
		t.Fatalf("тело не разобрано: %v; тело=%q", err, gotBody)
	}
	if p.Event != "on_waiting" {
		t.Errorf("event = %q, ожидался on_waiting", p.Event)
	}
	if p.Pipeline != "examples/p.yaml" {
		t.Errorf("pipeline = %q, ожидался examples/p.yaml", p.Pipeline)
	}
	if p.Detail == "" {
		t.Error("detail пуст — получатель не узнает, что произошло")
	}
	if p.At == "" {
		t.Error("at пуст — время события обязано быть, иначе сортировать инциденты нечем")
	}
}

// TestFireAlertReportsNon2xx — «доставили» и «доставлено» не одно и то же.
// Ответ 500 должен стать ошибкой, иначе демон считает оповещение отправленным.
func TestFireAlertReportsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sched := &pipeline.Schedule{Alerts: pipeline.ScheduleAlerts{Webhook: srv.URL}}
	err := fireAlert(sched, "on_failure", "examples/p.yaml", "boom")
	if err == nil {
		t.Fatal("ответ 500 должен давать ошибку, а не успех")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("в ошибке нет кода ответа: %v", err)
	}
}

// TestFireAlertRejectsNonHTTPWebhook — мусор в конфиге должен быть назван, а не
// отправлен в сеть как есть.
func TestFireAlertRejectsNonHTTPWebhook(t *testing.T) {
	sched := &pipeline.Schedule{Alerts: pipeline.ScheduleAlerts{Webhook: "не урл вообще"}}
	err := fireAlert(sched, "on_waiting", "examples/p.yaml", "d")
	if err == nil {
		t.Fatal("не-http вебхук должен отвергаться")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("в ошибке нет указания, что ожидался http(s): %v", err)
	}
}

// TestFireAlertSubscriptionWithoutChannelFails — главный сценарий из ревью:
// подписка есть, канала доставки нет. Молчать здесь нельзя: именно этот случай
// выглядит как «алерты настроены», хотя уйти им некуда.
func TestFireAlertSubscriptionWithoutChannelFails(t *testing.T) {
	sched := &pipeline.Schedule{Alerts: pipeline.ScheduleAlerts{
		OnWaiting: "telegram_notifier", // такого плагина в реестре нет
	}}
	err := fireAlert(sched, "on_waiting", "examples/p.yaml", "d")
	if err == nil {
		t.Fatal("подписка без канала доставки обязана давать ошибку, а не nil")
	}
	if !strings.Contains(err.Error(), "telegram_notifier") {
		t.Errorf("в ошибке нет имени нотификатора: %v", err)
	}
}

// TestFireAlertSilentWithoutSubscription — тишина без подписки это норма, а не
// поломка. Демон не должен сыпать ошибками пайплайны без alerts.
func TestFireAlertSilentWithoutSubscription(t *testing.T) {
	sched := &pipeline.Schedule{}
	if err := fireAlert(sched, "on_waiting", "examples/p.yaml", "d"); err != nil {
		t.Errorf("без подписки ошибки быть не должно, получено: %v", err)
	}
	if err := fireAlert(nil, "on_waiting", "examples/p.yaml", "d"); err != nil {
		t.Errorf("при nil-расписании ошибки быть не должно, получено: %v", err)
	}
}

// TestFireAlertWebhookWithoutSubscriptionStillFires — обратная сторона: если
// канал задан, событие уходит даже без явной подписки на этот вид. Иначе
// пришлось бы дублировать `on_waiting: "1"` рядом с `webhook:` ради одного
// и того же POST.
func TestFireAlertWebhookWithoutSubscriptionStillFires(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sched := &pipeline.Schedule{Alerts: pipeline.ScheduleAlerts{Webhook: srv.URL}}
	if err := fireAlert(sched, "on_waiting", "examples/p.yaml", "d"); err != nil {
		t.Fatalf("ошибка при заданном вебхуке: %v", err)
	}
	if hits != 1 {
		t.Errorf("вебхук вызван %d раз, ожидался 1", hits)
	}
}
