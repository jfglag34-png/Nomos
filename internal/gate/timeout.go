package gate

import (
	"errors"
	"io"
	"time"
)

// ErrGateTimeout — человек не ответил за отведённое время.
//
// Отдельная ошибка, а не ErrClosed: «человек закрыл вкладку» и «человек не
// пришёл» — разные события, и в журнале рана они должны читаться по-разному.
// Иначе отчёт о прогоне не отличил бы отмену человека от тихого висящего ранa.
var ErrGateTimeout = errors.New("гейт: ожидание решения истекло")

// Timeout policy: что делать, когда время вышло.
//
// Константы строк, а не bool: в YAML это читается как есть, и «abort|auto_approve»
// в конфиге понятнее, чем загадочный `on_timeout: true`.
const (
	// TimeoutAbort — сорвать ожидание, ран завершится ошибкой.
	TimeoutAbort = "abort"
	// TimeoutAutoApprove — ответить за человека. Опасно по определению, и
	// включается только явным словом в конфиге.
	TimeoutAutoApprove = "auto_approve"
)

// TimeoutUI — структурированный ввод гейта с потолком ожидания.
//
// Проблема, которую он снимает: демон запускает пайплайн с human_gate: wait и
// уходит в WaitDecision. Решение приходит из браузера или из `wedra approve`,
// а если никто не придёт — ран висит МОЛЧА. Для unattended-сервера это худший
// из failure mode: парсер встал на гейт в три ночи, ждёт вечно, и узнать об
// этом можно только по тому, что прогона не случилось.
//
// Почему обёртка, а не гонка в WaitDecision канала: Close() у ChannelUI
// терминален и означает «человек ушёл», а таймаут — это «человек не пришёл
// вовремя». Смешивать их в один сигнал нельзя, иначе отчёт о прогоне соврёт.
//
// Утечка горутины исключена: по истечении времени внутренний ввод закрывается
// через Close, если он это умеет, иначе ждущая горутина остаётся висеть, но не
// удерживает ничего, кроме памяти, и не блокирует процесс.
type TimeoutUI struct {
	inner     StructuredUI
	timeout   time.Duration
	onTimeout string
}

// NewTimeoutUI — обёртка с дедлайном.
//
// timeout <= 0 означает «ждать как раньше»: для одиночного запуска в терминале
// ограничение не нужно, и молча добавлять его всем было бы изменением
// поведения без спроса.
func NewTimeoutUI(inner StructuredUI, timeout time.Duration, onTimeout string) *TimeoutUI {
	return &TimeoutUI{inner: inner, timeout: timeout, onTimeout: onTimeout}
}

// WaitDecision — ждёт решения, но не дольше потолка.
func (t *TimeoutUI) WaitDecision() (Decision, error) {
	if t.inner == nil {
		return Decision{}, ErrClosed
	}
	if t.timeout <= 0 {
		return t.inner.WaitDecision()
	}
	type result struct {
		d   Decision
		err error
	}
	// Буфер на 1: горутина должна завершиться даже после того, как мы ушли по
	// таймауту и никто больше не читает её результат.
	done := make(chan result, 1)
	go func() {
		d, err := t.inner.WaitDecision()
		done <- result{d, err}
	}()
	select {
	case r := <-done:
		return r.d, r.err
	case <-time.After(t.timeout):
		// Освобождаем внутреннее ожидание, если ввод это умеет.
		if c, ok := t.inner.(interface{ Close() bool }); ok {
			c.Close()
		}
		if t.onTimeout == TimeoutAutoApprove {
			return Decision{Action: "approve", Source: "gate-timeout"}, nil
		}
		return Decision{}, ErrGateTimeout
	}
}

// ReadLine — построчный режим не используется: Service идёт через
// StructuredUI. Если терминальный путь всё же достанется сюда, честный EOF
// вместо чтения чужого stdin.
func (t *TimeoutUI) ReadLine() (string, error) { return "", io.EOF }
