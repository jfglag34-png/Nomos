package cron

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// shellForTest — оболочка для заведомо короткого процесса.
func shellForTest() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

// exitArgsForTest — аргументы завершения.
//
// Именно ОТДЕЛЬНЫМИ аргументами: `sh "-c exit 0"` — это один аргумент, и sh
// такой не понимает (он ждёт -c и команду раздельно). На Windows cmd такой
// вызов терпит, а на Linux и macOS — нет, и тест падал бы только там.
func exitArgsForTest() []string {
	if runtime.GOOS == "windows" {
		return []string{"/c", "exit 0"}
	}
	return []string{"-c", "exit 0"}
}

// TestTryLockTakesOverDeadOwner — блокировка, владелец которой мёртв,
// забирается.
//
// Это не косметика: блокировка демона снимается через defer, а убитый
// процесс (SIGTERM, паника, OOM) defer не выполняет. Без проверки протухания
// демон после рестарта не стартует НИКОГДА, и это выглядит как «планировщик
// молча не работает».
func TestTryLockTakesOverDeadOwner(t *testing.T) {
	dir := t.TempDir()
	lp := lockPath(dir)
	// pid из заведомо завершившегося процесса: гарантированно мёртвый.
	// Произвольное большое число брать нельзя — на Windows OpenProcess может
	// ответить успехом на pid, которого нет.
	dead := exec.Command(shellForTest(), exitArgsForTest()...)
	if err := dead.Run(); err != nil {
		t.Fatalf("вспомогательный процесс: %v", err)
	}
	pid := dead.Process.Pid
	if err := os.WriteFile(lp, []byte(strconv.Itoa(pid)+" 2026-10-19T00:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, ok := tryLock(dir)
	if !ok {
		t.Fatal("протухшая блокировка не была забрана — демон больше не стартует")
	}
	defer unlock()
	if raw, err := os.ReadFile(lp); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("после перехвата в файле: %q, наш pid=%d", string(raw), os.Getpid())
	}
	// После перехвата блокировка принадлежит нам, то есть протухшей быть НЕ
	// должна. Условие именно такое: протухшая — это плохо.
	if staleLock(lp) {
		t.Error("после перехвата блокировка всё ещё протухшая — записан не наш pid")
	}
}

// TestTryLockKeepsLiveOwner — живой блокировщик не вытесняется.
func TestTryLockKeepsLiveOwner(t *testing.T) {
	dir := t.TempDir()
	unlock, ok := tryLock(dir)
	if !ok {
		t.Fatal("первая блокировка не взялась")
	}
	defer unlock()
	if _, ok := tryLock(dir); ok {
		t.Error("вторая блокировка взялась, хотя первый владелец жив")
	}
}

// TestStaleLockUnreadable — нечитаемая блокировка считается протухшей, иначе
// демон заблокирован навсегда.
func TestStaleLockUnreadable(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "broken.lock")
	if err := os.WriteFile(lp, []byte("не pid вовсе"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !staleLock(lp) {
		t.Error("нечитаемая блокировка не признана протухшей")
	}
}
