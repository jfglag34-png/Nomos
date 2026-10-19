package gate

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// newApprover — подписатель и реестр для тестов.
func newApprover(t *testing.T) *Approver {
	t.Helper()
	s, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	return NewApprover(s, "http://127.0.0.1:8765", time.Hour)
}

// tokenFromLink — токен из выданной ссылки.
func tokenFromLink(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("t")
	if tok == "" {
		t.Fatalf("в ссылке нет токена: %s", link)
	}
	return tok
}

// TestApproveDeliversDecision — ссылка доставляет решение ожидающему гейту.
func TestApproveDeliversDecision(t *testing.T) {
	a := newApprover(t)
	ui := NewChannelUI()
	link, err := a.Register("run-1", "step-1", ui, &pipeline.Step{ID: "step-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "http://127.0.0.1:8765/approve?t=") {
		t.Errorf("ссылка не похожа на раздаваемую: %s", link)
	}
	if err := a.Deliver(tokenFromLink(t, link), "accept", nil); err != nil {
		t.Fatalf("доставка решения: %v", err)
	}
	select {
	case sig := <-ui.ch:
		if sig.d.Action != "accept" {
			t.Errorf("действие = %q, ожидалось accept", sig.d.Action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("решение не дошло до гейта")
	}
}

// TestApproveLinkIsSingleUse — одна ссылка, одно решение. Иначе ссылка,
// попавшая в чужой канал, одобряла бы гейт дважды и недолго.
func TestApproveLinkIsSingleUse(t *testing.T) {
	a := newApprover(t)
	ui := NewChannelUI()
	link, err := a.Register("run-1", "step-1", ui, &pipeline.Step{ID: "step-1"})
	if err != nil {
		t.Fatal(err)
	}
	tok := tokenFromLink(t, link)
	if err := a.Deliver(tok, "accept", nil); err != nil {
		t.Fatal(err)
	}
	<-ui.ch
	if err := a.Deliver(tok, "accept", nil); !errors.Is(err, ErrApproveUsed) {
		t.Fatalf("повторная доставка: ошибка = %v, ожидалась ErrApproveUsed", err)
	}
}

// TestApproveRejectsForeignSignature — ссылка, подписанная чужим ключом, не
// работает. Это и есть защита от подсовывания.
func TestApproveRejectsForeignSignature(t *testing.T) {
	a := newApprover(t)
	other, _ := NewSigner()
	foreign, err := other.Sign(Link{RunID: "run-1", StepID: "step-1", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	ui := NewChannelUI()
	if _, err := a.Register("run-1", "step-1", ui, &pipeline.Step{ID: "step-1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Deliver(foreign, "accept", nil); !errors.Is(err, ErrLinkBadSignature) {
		t.Fatalf("ошибка = %v, ожидалась ErrLinkBadSignature", err)
	}
}

// TestApproveRejectsExpiredLink — просроченная ссылка не работает.
func TestApproveRejectsExpiredLink(t *testing.T) {
	s, _ := NewSigner()
	a := NewApprover(s, "http://x", time.Hour)
	ui := NewChannelUI()
	expired, _ := s.Sign(Link{RunID: "run-1", StepID: "step-1", Expires: time.Now().Add(-time.Minute)})
	if _, err := a.Register("run-1", "step-1", ui, &pipeline.Step{ID: "step-1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Deliver(expired, "accept", nil); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("ошибка = %v, ожидалась ErrLinkExpired", err)
	}
}

// TestApproveUnknownGate — ссылка на гейт, которого уже нет, не проходит.
func TestApproveUnknownGate(t *testing.T) {
	a := newApprover(t)
	ui := NewChannelUI()
	link, _ := a.Register("run-1", "step-1", ui, &pipeline.Step{ID: "step-1"})
	a.Unregister("run-1", "step-1")
	if err := a.Deliver(tokenFromLink(t, link), "accept", nil); !errors.Is(err, ErrApproveUnknownGate) {
		t.Fatalf("ошибка = %v, ожидалась ErrApproveUnknownGate", err)
	}
}

// TestApproveRejectsActionOutsideList — решение не из списка гейта не
// принимается.
func TestApproveRejectsActionOutsideList(t *testing.T) {
	a := newApprover(t)
	ui := NewChannelUI()
	link, _ := a.Register("run-1", "step-1", ui, &pipeline.Step{ID: "step-1", Actions: []string{"accept"}})
	err := a.Deliver(tokenFromLink(t, link), "reject", nil)
	if !errors.Is(err, ErrApproveBadAction) {
		t.Fatalf("ошибка = %v, ожидалась ErrApproveBadAction", err)
	}
}

// TestApproveHTTPFlowGetThenPost — страница показывает форму, POST доставляет
// решение. Это тот путь, которым пойдёт человек с телефона.
func TestApproveHTTPFlowGetThenPost(t *testing.T) {
	a := newApprover(t)
	ui := NewChannelUI()
	step := &pipeline.Step{
		ID:      "gate1",
		Actions: []string{"accept", "reject"},
		Form:    []pipeline.FormField{{Field: "comment", Editable: true, Type: "string"}},
	}
	link, _ := a.Register("run-1", "gate1", ui, step)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// GET — страница с формой.
	resp, err := http.Get(srv.URL + "/approve?t=" + url.QueryEscape(tokenFromLink(t, link)))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET вернул %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "comment") || !strings.Contains(body, "accept") {
		t.Errorf("страница не содержит формы гейта: %s", body)
	}

	// POST — решение с заполненным полем.
	form := url.Values{"action": {"accept"}, "comment": {"ок"}}
	resp2, err := http.PostForm(srv.URL+"/approve?t="+url.QueryEscape(tokenFromLink(t, link)), form)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("POST вернул %d: %s", resp2.StatusCode, readAll(t, resp2))
	}
	select {
	case sig := <-ui.ch:
		if sig.d.Action != "accept" {
			t.Errorf("действие = %q", sig.d.Action)
		}
		if sig.d.Edits["comment"] != "ок" {
			t.Errorf("правки = %v, ожидался comment=ок", sig.d.Edits)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("решение не дошло до гейта")
	}
}

// TestApproveHTTPRejectsBadToken — страница с чужой подписью не отдаёт форму.
func TestApproveHTTPRejectsBadToken(t *testing.T) {
	a := newApprover(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/approve?t=" + url.QueryEscape("мусор"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		t.Error("мусорный токен не должен давать страницу одобрения")
	}
}

// readAll — тело ответа строкой.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}
