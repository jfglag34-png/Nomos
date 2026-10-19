package cli

import (
	"os"
	"strings"
)

// Nomos keeps the inherited WEDRA namespace compatible. Explicit NOMOS values,
// including empty ones, take precedence. Run this once before starting workers.
func applyNomosEnvironment() {
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, "NOMOS_") {
			_ = os.Setenv("WEDRA_"+strings.TrimPrefix(name, "NOMOS_"), value)
		}
	}
}
