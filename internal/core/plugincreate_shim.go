package core

import pc "github.com/wykserdex/wedra/internal/plugincreate"

// Shim: public API of plugincreate remains via core for backwards compat.
// Новые пакеты импортируют "internal/plugincreate" напрямую.

// CreateOptions re-export
type CreateOptions = pc.CreateOptions

func ParseCreateArgs(args []string) (string, CreateOptions, bool, error) {
	return pc.ParseCreateArgs(args)
}
func CreatePlugin(dir string) (string, error) { return pc.CreatePlugin(dir) }
func CreatePluginWith(dir string, opts CreateOptions) (string, error) {
	return pc.CreatePluginWith(dir, opts)
}
func ListTemplates() string { return pc.ListTemplates() }
func TemplateNames() []string { return pc.TemplateNames() }
func PyLiteralForTest(s string) (string, error) { return pc.PyLiteralForTest(s) }
