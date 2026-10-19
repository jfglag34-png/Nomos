package cli

import (
	"os"
	"testing"
)

func TestNomosEnvironmentCompatibility(t *testing.T) {
	t.Setenv("WEDRA_TRIGGER_TOKEN", "legacy")
	t.Setenv("NOMOS_TRIGGER_TOKEN", "preferred")
	t.Setenv("WEDRA_ALERT_WEBHOOK", "legacy-url")
	t.Setenv("NOMOS_ALERT_WEBHOOK", "")
	applyNomosEnvironment()
	if os.Getenv("WEDRA_TRIGGER_TOKEN") != "preferred" {
		t.Fatal("NOMOS must win")
	}
	if os.Getenv("WEDRA_ALERT_WEBHOOK") != "" {
		t.Fatal("explicit empty NOMOS must disable legacy value")
	}
}
