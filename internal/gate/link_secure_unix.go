//go:build !windows

package gate

import (
	"fmt"
	"os"
	"path/filepath"
)

// secureWriteFile — запись файла, доступного только владельцу.
//
// Chmod после создания обязателен: os.WriteFile применяет umask, а umask
// может забрать права у владельца (обычно 0600 и так, но «обычно» — не
// «всегда»), и тогда демон не сможет перечитать свой же ключ.
func secureWriteFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("права ключа подписи %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" {
		// Каталог с ключом тоже не должен быть доступен группе и остальным:
		// иначе заменить файл ключа можно, не имея прав на сам файл.
		_ = os.Chmod(dir, 0o700)
	}
	return nil
}
