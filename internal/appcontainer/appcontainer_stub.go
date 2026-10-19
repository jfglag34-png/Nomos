//go:build !windows

package appcontainer

import (
	"context"
	"fmt"
	"runtime"
)

// Prepare — на не-Windows платформах изолятор другой (на Linux это bwrap, см.
// internal/plugin/sandbox_linux.go), поэтому здесь отказ.
func Prepare(Plan) error {
	return fmt.Errorf("%w: платформа %s", ErrUnsupported, runtime.GOOS)
}

// Run — см. Prepare.
func Run(context.Context, Plan) (Result, error) {
	return Result{}, fmt.Errorf("%w: платформа %s", ErrUnsupported, runtime.GOOS)
}

// DeleteProfile — профилей AppContainer на этой платформе не бывает.
func DeleteProfile(string) error {
	return fmt.Errorf("%w: платформа %s", ErrUnsupported, runtime.GOOS)
}

// Probe — изолятор тут не бывает, поэтому и проверять нечего.
func Probe() error {
	return fmt.Errorf("%w: платформа %s", ErrUnsupported, runtime.GOOS)
}
