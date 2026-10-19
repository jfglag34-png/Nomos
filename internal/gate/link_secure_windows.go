//go:build windows

package gate

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// secureWriteFile — запись файла, доступного только владельцу.
//
// На Windows права POSIX не работают: os.WriteFile их просто игнорирует, а
// файл наследует ACL каталога. Ключ подписи — это и есть право одобрять
// гейты, поэтому ACL ставится явно: только владелец файла и SYSTEM.
//
// PROTECTED_DACL_SECURITY_INFORMATION здесь обязателен: без него запись
// ПРОПУСКАЕТСЯ, если у каталога есть унаследуемые записи, и файл снова
// получает права каталога. Это ровно тот случай, который в проекте уже
// ловился: «выдача прав запрещена, а на самом деле просто забыли флаг».
func secureWriteFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("создание ключа подписи: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("запись ключа подписи: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("закрытие ключа подписи: %w", err)
	}

	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("токен процесса: %w", err)
	}
	user, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("SID владельца: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("SID SYSTEM: %w", err)
	}
	mk := func(sid *windows.SID) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{mk(user.User.Sid), mk(system)}, nil)
	if err != nil {
		return fmt.Errorf("сборка ACL ключа: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("установка ACL ключа подписи: %w", err)
	}
	return nil
}
