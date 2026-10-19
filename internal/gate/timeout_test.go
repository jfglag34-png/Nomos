package gate

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStructured — ввод гейта под управлением теста: решение либо приходит по
// требованию, либо не приходит никогда.
type fakeStructured struct {
	decision Decision
	err      error
	release  chan struct{} // если nil — WaitDecision возвращает сразу
	closed   int32
}

func (f *fakeStructured) WaitDecision() (Decision, error) {
	if f.release != nil {
		<-f.release
	}
	return f.decision, f.err
}

func (f *fakeStructured) Close() bool { atomic.StoreInt32(&f.closed, 1); return true }

// TestTimeoutUIDeliversDecisionBeforeDeadline — обычный случай: человек ответил,
// потолок не тронут.
func TestTimeoutUIDeliversDecisionBeforeDeadline(t *testing.T) {
	inner := &fakeStructured{decision: Decision{Action: "approve"}}
	ui := NewTimeoutUI(inner, time.Second, TimeoutAbort)
	d, err := ui.WaitDecision()
	if err != nil {
		t.Fatalf("ошибка до истечения потолка: %v", err)
	}
	if d.Action != "approve" {
		t.Errorf("решение = %q, ожидалось approve", d.Action)
	}
}

// TestTimeoutUIAbort — главный случай: человек не пришёл. Раньше ран висел
// молча, теперь это ошибка с внятным текстом.
func TestTimeoutUIAbort(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	inner := &fakeStructured{release: release}

	ui := NewTimeoutUI(inner, 40*time.Millisecond, TimeoutAbort)
	start := time.Now()
	_, err := ui.WaitDecision()
	elapsed := time.Since(start)

	if !errors.Is(err, ErrGateTimeout) {
		t.Fatalf("ошибка = %v, ожидалась ErrGateTimeout", err)
	}
	if elapsed > time.Second {
		t.Errorf("ждали %v, потолок 40мс — значит таймаут не сработал", elapsed)
	}
}

// TestTimeoutUIAutoApprove — противоположная политика: ответить за человека.
// Опасна по определению, поэтому включается только явным словом.
func TestTimeoutUIAutoApprove(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	inner := &fakeStructured{release: release}

	ui := NewTimeoutUI(inner, 40*time.Millisecond, TimeoutAutoApprove)
	d, err := ui.WaitDecision()
	if err != nil {
		t.Fatalf("auto_approve не должен давать ошибку, получено: %v", err)
	}
	if d.Action != "approve" {
		t.Errorf("решение = %q, ожидалось approve", d.Action)
	}
	if d.Source != "gate-timeout" {
		t.Errorf("source = %q — по нему видно, что решение принято по таймауту, а не человеком", d.Source)
	}
}

// TestTimeoutUIDefaultIsAbort — любое значение, кроме явного auto_approve,
// трактуется как abort. Молча одобрять за человека нельзя.
func TestTimeoutUIDefaultIsAbort(t *testing.T) {
	for _, onTimeout := range []string{"", "abort", "auto-accept", "да"} {
		release := make(chan struct{})
		inner := &fakeStructured{release: release}
		ui := NewTimeoutUI(inner, 30*time.Millisecond, onTimeout)
		_, err := ui.WaitDecision()
		close(release)
		if !errors.Is(err, ErrGateTimeout) {
			t.Errorf("on_timeout=%q: ошибка = %v, ожидался ErrGateTimeout", onTimeout, err)
		}
	}
}

// TestTimeoutUIClosesInnerOnTimeout — по истечении потолка внутреннее ожидание
// освобождается. Иначе на каждый гейт остаётся висящая горутина, а их
// accumulates на длинном прогоне.
func TestTimeoutUIClosesInnerOnTimeout(t *testing.T) {
	release := make(chan struct{})
	inner := &fakeStructured{release: release}

	ui := NewTimeoutUI(inner, 30*time.Millisecond, TimeoutAbort)
	if _, err := ui.WaitDecision(); !errors.Is(err, ErrGateTimeout) {
		t.Fatalf("ожидалась ErrGateTimeout, получено %v", err)
	}
	if atomic.LoadInt32(&inner.closed) != 1 {
		t.Error("внутренний ввод не закрыт по таймауту — ждущая горутина останется висеть")
	}
	close(release)
}

// TestTimeoutUIZeroKeepsOldBehaviour — потолок не задан = ждать как раньше.
// В терминале человек перед тобой, и молча добавлять ему лимит значило бы
// менять поведение без спроса.
func TestTimeoutUIZeroKeepsOldBehaviour(t *testing.T) {
	inner := &fakeStructured{decision: Decision{Action: "reject"}, err: errors.New("поток закрыт")}
	ui := NewTimeoutUI(inner, 0, TimeoutAbort)
	d, err := ui.WaitDecision()
	if err == nil {
		t.Fatal("ожидалась ошибка внутреннего ввода")
	}
	if d.Action != "reject" {
		t.Errorf("решение = %q, ожидалось reject", d.Action)
	}
	if atomic.LoadInt32(&inner.closed) != 0 {
		t.Error("при нулевом потолке закрывать внутренний ввод нельзя — он живёт дальше")
	}
}

// TestTimeoutUINilInner — отсутствие ввода это ошибка, а не паника: nil-интерфейс
// иначе уронил бы ран.
func TestTimeoutUINilInner(t *testing.T) {
	ui := NewTimeoutUI(nil, time.Second, TimeoutAbort)
	if _, err := ui.WaitDecision(); err == nil {
		t.Fatal("nil-ввод должен давать ошибку, а не nil")
	}
}

// TimeoutUI обязан удовлетворять StructuredUI — ран выбирает структурированный
// путь по интерфейсу, а не по типу.
//
// Проверка именно компиляционная: рантайм-сравнение `var ui StructuredUI =
// NewTimeoutUI(...)` с nil всегда истинно иstaticcheck справедливо роняет такой
// код как SA4023. Если TimeoutUI перестанет удовлетворять интерфейсу, сборка
// должна не пройти — иначе таймаут молча выключится и всё вернётся к висящему
// ранy без единого красного теста.
var _ StructuredUI = (*TimeoutUI)(nil)
