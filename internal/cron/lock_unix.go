//go:build !windows

package cron

import (
	"syscall"
)

// processAlive — жив ли процесс с таким pid.
//
// Сигнал 0 ничего не отправляет, а только проверяет наличие: ESRCH — процесса
// нет, EPERM — он существует, но нашему пользователю не по зубам (и это тоже
// «жив»).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
