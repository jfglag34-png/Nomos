//go:build windows

package cron

import (
	"golang.org/x/sys/windows"
)

// processAlive — жив ли процесс с таким pid.
//
// На Windows сигнала 0 нет, поэтому проверка идёт через OpenProcess: если
// процесс есть, хэндл открывается; если нет — ошибка ERROR_INVALID_PARAMETER
// (неверный pid). Права PROCESS_QUERY_LIMITED_INFORMATION достаточно, чтобы
// спросить «существуешь», и не требуют прав администратора.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}
