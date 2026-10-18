package core

import (
	stdctx "context"
	pt "github.com/wykserdex/wedra/internal/plugintest"
)

// Shim: public API plugintest остаётся через core

type TestService = pt.TestService
type PluginTestFile = pt.PluginTestFile
type PluginTestCase = pt.PluginTestCase
type Expectation = pt.Expectation
type ErrorExpectation = pt.ErrorExpectation
type CaseResult = pt.CaseResult

func RunPluginTests(dir, specPath string, quiet bool) (int, int, error) {
	return pt.RunPluginTests(dir, specPath, quiet)
}
func RunPluginTestsCtx(ctx stdctx.Context, dir, specPath string, quiet bool) (int, int, error) {
	return pt.RunPluginTestsCtx(ctx, dir, specPath, quiet)
}
