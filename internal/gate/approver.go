package gate

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// Ошибки доставки решения.
var (
	// ErrApproveUnknownGate — по ссылке нет ожидающего гейта: ран ушёл дальше,
	// умер или ссылка от другого гейта.
	ErrApproveUnknownGate = errors.New("ожидающего гейта нет")
	// ErrApproveUsed — ссылка уже использована.
	ErrApproveUsed = errors.New("ссылка уже использована")
	// ErrApproveBadAction — действие не из списка гейта.
	ErrApproveBadAction = errors.New("действие не из списка гейта")
)

// pendingGate — гейт, ждущий решения, и выданная на него ссылка.
type pendingGate struct {
	runID   string
	stepID  string
	ui      *ChannelUI
	step    *pipeline.Step
	link    string
	expires time.Time
}

// Approver — раздача ссылок одобрения гейтов.
//
// Зачем он нужен: гейт, ждущий человека, должен быть одобрим человеком, а
// не тем, кто сидит за терминалом сервера. Раньше путь был один — интерактивный
// TTY, — и на безголовой машине он не работал вовсе: демон ждал вечно, и
// одобрить было некому.
//
// Ссылка здесь — и есть право одобрения: она подписана ключом демона, живёт
// ограниченное время и работает один раз. Всё остальное (кто её выдал,
// кому она отправлена) значения не имеет: получивший ссылку человек и есть
// тот, кто решает.
type Approver struct {
	signer  *Signer
	baseURL string
	ttl     time.Duration

	mu      sync.Mutex
	pending map[string]*pendingGate
	burned  map[string]bool
}

// NewApprover — реестр гейтов.
//
// baseURL — адрес, по которому человек откроет ссылку. Он же попадает в
// ссылку, поэтому ошибиться в нём нельзя: ссылка уйдёт в никуда.
func NewApprover(signer *Signer, baseURL string, ttl time.Duration) *Approver {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Approver{
		signer:  signer,
		baseURL: strings.TrimRight(baseURL, "/"),
		ttl:     ttl,
		pending: map[string]*pendingGate{},
		burned:  map[string]bool{},
	}
}

// gateKey — ключ гейта в реестре: ран и шаг.
func gateKey(runID, stepID string) string { return runID + "|" + stepID }

// Register — зарегистрировать ожидающий гейт и выдать на него ссылку.
//
// Ссылка возвращается вызывающему: демон печатает её, и человек открывает её
// где угодно — с телефона, из мессенджера, с другой машины.
func (a *Approver) Register(runID, stepID string, ui *ChannelUI, step *pipeline.Step) (string, error) {
	if a == nil || a.signer == nil {
		return "", errors.New("подписатель не задан")
	}
	if runID == "" || stepID == "" {
		return "", errors.New("гейт без рана или шага")
	}
	expires := time.Now().Add(a.ttl)
	token, err := a.signer.Sign(Link{RunID: runID, StepID: stepID, Expires: expires})
	if err != nil {
		return "", err
	}
	link := a.baseURL + "/approve?t=" + url.QueryEscape(token)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[gateKey(runID, stepID)] = &pendingGate{
		runID: runID, stepID: stepID, ui: ui, step: step, link: link, expires: expires,
	}
	return link, nil
}

// Unregister — снять гейт: ран ушёл дальше, умер или был отменён.
//
// Ссылка при этом не «портится», а просто перестаёт finds: гейта, к которому
// она вела, больше нет, и решение по ней некуда доставить.
func (a *Approver) Unregister(runID, stepID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, gateKey(runID, stepID))
}

// Pending — сколько гейтов ждёт решения (для диагностики и тестов).
func (a *Approver) Pending() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Deliver — доставить решение по подписанной ссылке.
//
// Порядок проверок: подпись и срок (иначе подделка выглядела бы как
// «ссылка истекла»), потом «уже использована», потом «есть ли такой гейт».
// Ссылка одноразовая: после доставки она сгорает, даже если доставка не
// удалась — повторять решение по той же ссылке нельзя.
func (a *Approver) Deliver(token, action string, edits map[string]interface{}) error {
	if a == nil || a.signer == nil {
		return errors.New("подписатель не задан")
	}
	l, err := a.signer.Verify(token, time.Now())
	if err != nil {
		return err
	}
	a.mu.Lock()
	key := gateKey(l.RunID, l.StepID)
	if a.burned[token] {
		a.mu.Unlock()
		return ErrApproveUsed
	}
	pg, ok := a.pending[key]
	if !ok {
		a.mu.Unlock()
		return ErrApproveUnknownGate
	}
	a.burned[token] = true
	a.mu.Unlock()

	if !allowedAction(pg, action) {
		return fmt.Errorf("%w: %q", ErrApproveBadAction, action)
	}
	if !pg.ui.SendDecision(Decision{Action: action, Edits: edits}) {
		return ErrApproveUnknownGate
	}
	return nil
}

// allowedAction — действие входит в список гейта.
//
// Пустой список означает «прими любой из accept/reject»: шаг без явного
// actions — обычный случай, и не принимать решение было бы глупостью.
func allowedAction(pg *pendingGate, action string) bool {
	if action == "" {
		return false
	}
	if pg.step == nil || len(pg.step.Actions) == 0 {
		return action == "accept" || action == "reject"
	}
	for _, a := range pg.step.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// Handler — HTTP-раздача ссылок одобрения.
//
// Два метода: GET показывает форму гейта, POST принимает решение. Никакой
// сессии, никакого cookie: подписанная ссылка и есть право.
func (a *Approver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/approve", a.handleApprove)
	return mux
}

func (a *Approver) handleApprove(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	if token == "" {
		writeApprovePage(w, http.StatusBadRequest, "Ссылка без токена", "")
		return
	}
	l, err := a.signer.Verify(token, time.Now())
	if err != nil {
		writeApprovePage(w, http.StatusGone, "Ссылка не работает: "+err.Error(), "")
		return
	}
	a.mu.Lock()
	pg, ok := a.pending[gateKey(l.RunID, l.StepID)]
	a.mu.Unlock()
	if !ok {
		writeApprovePage(w, http.StatusGone, "Этот гейт уже не ждёт решения", "")
		return
	}

	if r.Method == http.MethodGet {
		writeApprovePage(w, http.StatusOK, "Одобрение шага "+pg.stepID, approveFormHTML(pg, token))
		return
	}
	if r.Method != http.MethodPost {
		writeApprovePage(w, http.StatusMethodNotAllowed, "Метод не поддерживается", "")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeApprovePage(w, http.StatusBadRequest, "Форма не разобрана", "")
		return
	}
	action := r.FormValue("action")
	edits := map[string]interface{}{}
	if pg.step != nil {
		for _, f := range pg.step.Form {
			if !f.Editable {
				continue
			}
			if v := strings.TrimSpace(r.FormValue(f.Field)); v != "" {
				edits[f.Field] = v
			}
		}
	}
	if err := a.Deliver(token, action, edits); err != nil {
		writeApprovePage(w, http.StatusConflict, "Решение не принято: "+err.Error(), "")
		return
	}
	a.Unregister(pg.runID, pg.stepID)
	writeApprovePage(w, http.StatusOK, "Решение принято: "+action, "")
}

// approveFormHTML — форма решения: поля гейта и кнопки.
func approveFormHTML(pg *pendingGate, token string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<form method="post" action="/approve?t=%s">`, html.EscapeString(token))
	if pg.step != nil && len(pg.step.Form) > 0 {
		b.WriteString(`<p>Поля шага:</p><table>`)
		for _, f := range pg.step.Form {
			kind := "text"
			switch f.Type {
			case "number", "integer":
				kind = "number"
			case "boolean", "bool":
				kind = "checkbox"
			}
			fmt.Fprintf(&b, `<tr><td>%s</td><td><input name="%s" type="%s"`,
				html.EscapeString(f.Field), html.EscapeString(f.Field), kind)
			if !f.Editable {
				b.WriteString(` disabled`)
			}
			b.WriteString(`></td></tr>`)
		}
		b.WriteString(`</table>`)
	}
	actions := []string{"accept", "reject"}
	if pg.step != nil && len(pg.step.Actions) > 0 {
		actions = append([]string{}, pg.step.Actions...)
		sort.Strings(actions)
	}
	for _, act := range actions {
		fmt.Fprintf(&b, `<p><button type="submit" name="action" value="%s">%s</button></p>`,
			html.EscapeString(act), html.EscapeString(act))
	}
	b.WriteString(`</form>`)
	return b.String()
}

// writeApprovePage — страница ответа. Минимум разметки: это служебная
// страница, а не витрина.
func writeApprovePage(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><html lang="ru"><head><meta charset="utf-8">`+
		`<title>%s</title></head><body><h1>%s</h1>%s</body></html>`,
		html.EscapeString(title), html.EscapeString(title), body)
}
