package gate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestLinkRoundTrip — подписанная ссылка проверяется своим же ключом.
func TestLinkRoundTrip(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	want := Link{RunID: "20261019-120000-x", StepID: "gate1", Expires: time.Now().Add(time.Hour)}
	tok, err := s.Sign(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(tok, time.Now())
	if err != nil {
		t.Fatalf("подписанная ссылка не проверилась: %v", err)
	}
	if got.RunID != want.RunID || got.StepID != want.StepID {
		t.Errorf("получено %+v, ожидалось %+v", got, want)
	}
}

// TestLinkRejectsForeignKey — чужой ключ не подходит. Это ровно тот случай,
// когда ссылка из чужого источника подсовывается человеку.
func TestLinkRejectsForeignKey(t *testing.T) {
	mine, _ := NewSigner()
	theirs, _ := NewSigner()
	tok, err := theirs.Sign(Link{RunID: "r", StepID: "s", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mine.Verify(tok, time.Now()); !errors.Is(err, ErrLinkBadSignature) {
		t.Fatalf("ошибка = %v, ожидалась ErrLinkBadSignature", err)
	}
}

// TestLinkRejectsTamperedPayload — правка полезной нагрузки ломает подпись.
func TestLinkRejectsTamperedPayload(t *testing.T) {
	s, _ := NewSigner()
	tok, err := s.Sign(Link{RunID: "run-a", StepID: "step-a", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Меняем run в нагрузке, подпись оставляем.
	parts := splitToken(tok)
	other, _ := NewSigner()
	_ = other
	bad, err := other.Sign(Link{RunID: "run-b", StepID: "step-a", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	tampered := parts[0] + "." + splitToken(bad)[1]
	if _, err := s.Verify(tampered, time.Now()); !errors.Is(err, ErrLinkBadSignature) {
		t.Fatalf("ошибка = %v, ожидалась ErrLinkBadSignature", err)
	}
}

// TestLinkRejectsExpired — просроченная ссылка не работает.
func TestLinkRejectsExpired(t *testing.T) {
	s, _ := NewSigner()
	tok, err := s.Sign(Link{RunID: "r", StepID: "s", Expires: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok, time.Now()); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("ошибка = %v, ожидалась ErrLinkExpired", err)
	}
}

// TestLinkRejectsMalformed — мусор не должен проходить как «ссылка».
func TestLinkRejectsMalformed(t *testing.T) {
	s, _ := NewSigner()
	for _, tok := range []string{"", "мусор", "a.b.c", "!!!.???"} {
		if _, err := s.Verify(tok, time.Now()); err == nil {
			t.Errorf("токен %q принят как ссылка", tok)
		}
	}
}

// TestLinkRequiresRunID — ссылка без рана не подписывается: подпись без
// привязки была бы «одобрите что угодно».
func TestLinkRequiresRunID(t *testing.T) {
	s, _ := NewSigner()
	if _, err := s.Sign(Link{StepID: "s", Expires: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("ссылка без run_id должна отвергаться")
	}
}

// TestLoadSignerPersists — ключ переживает перезапуск: ссылка, выданная до
// рестарта демона, должна остаться рабочей.
func TestLoadSignerPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "approve.key")
	s1, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s1.Sign(Link{RunID: "r", StepID: "s", Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadSigner(path)
	if err != nil {
		t.Fatalf("повторная загрузка ключа: %v", err)
	}
	if _, err := s2.Verify(tok, time.Now()); err != nil {
		t.Fatalf("ссылка перестала работать после перезапуска: %v", err)
	}
}

// TestLoadSignerWritesRestrictiveFile — ключ доступен только владельцу. Это
// и есть право одобрять гейты.
func TestLoadSignerWritesRestrictiveFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("на Windows права задаются ACL, а не POSIX-режимом — см. TestKeyDCLIsOwnerOnlyOnWindows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "approve.key")
	if _, err := LoadSigner(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("права ключа %o: группа и остальные не должны иметь доступа", mode)
	}
}
