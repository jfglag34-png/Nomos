//go:build windows

package gate

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestKeyDCLIsOwnerOnlyOnWindows — на Windows права ключа задаются ACL, а не
// POSIX-режимом, поэтому проверять надо именно DCL.
//
// Что проверяется: в DCL ровно две записи (владелец файла и SYSTEM) и DCL
// защищён (PROTECTED). Без PROTECTED файл наследует права каталога, и
// «закрытый» ключ оказывается доступен всем, кто может читать каталог.
func TestKeyDCLIsOwnerOnlyOnWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "approve.key")
	if _, err := LoadSigner(path); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatal("у ключа нет DCL — доступ не ограничен")
	}
	if got := dacl.AceCount; got != 2 {
		t.Errorf("в DCL %d записей, ожидались 2 (владелец и SYSTEM)", got)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Error("DCL ключа не защищён: файл наследует права каталога, и ключ доступен лишним")
	}
}

// TestKeyNotRewrittenOnSecondLoad — повторная загрузка ключа НЕ трогает файл.
//
// Это инвариант, а не деталь: secureWriteFile создаёт файл с O_EXCL и
// перезапись запрещает, а LoadSigner зовёт её только когда файла нет. Проверка
// ловит возврат к перезаписи: если бы ключ переписывался при каждой загрузке,
// то DCL вновь оказался бы с правами каталога — а с O_EXCL это ещё и сломало бы
// чтение ключа на живом демоне.
func TestKeyNotRewrittenOnSecondLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "approve.key")
	if _, err := LoadSigner(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(path); err != nil {
		t.Fatalf("повторная загрузка ключа: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("повторная загрузка изменила файл ключа")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount != 2 {
		t.Errorf("после повторной загрузки в DCL %v записей, ожидались 2", dacl)
	}
}
