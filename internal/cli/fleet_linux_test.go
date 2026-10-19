//go:build linux

package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetConfig(t *testing.T) {
	dir := t.TempDir()
	pipeline := filepath.Join(dir, "pipeline.yaml")
	os.WriteFile(pipeline, []byte("pipeline: {}"), 0600)
	path := filepath.Join(dir, "fleet.yaml")
	for _, tc := range []struct {
		tail string
		bad  bool
	}{
		{"", false}, {"\n    timeout: nonsense", true}, {"\n    cron: nonsense", true}, {"\n    timezone: Nowhere/No", true}, {"\n    typo: true", true},
	} {
		os.WriteFile(path, []byte("jobs:\n  - id: parser\n    file: "+pipeline+tc.tail+"\n"), 0600)
		c, e := loadFleet(path)
		if (e != nil) != tc.bad {
			t.Fatalf("%q: %v", tc.tail, e)
		}
		if e == nil && c.Concurrency != 6 {
			t.Fatal("default capacity")
		}
	}
}
func TestFleetAuthorization(t *testing.T) {
	t.Setenv("WEDRA_TRIGGER_TOKEN", "secret")
	f := &fleet{ctx: context.Background()}
	for _, tc := range []struct {
		path, auth string
		code       int
	}{{"/health", "", 401}, {"/health", "Bearer wrong", 401}, {"/health", "Bearer secret", 200}, {"/trigger/missing", "Bearer secret", 404}} {
		method := "GET"
		if strings.HasPrefix(tc.path, "/trigger/") {
			method = "POST"
		}
		r := httptest.NewRequest(method, tc.path, nil)
		r.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		f.handler().ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
	}
}
func TestFleetAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fleet{ctx: ctx, active: map[string]bool{"parser": true}, slots: make(chan struct{}, 1)}
	if f.start(fleetJob{ID: "parser"}) {
		t.Fatal("duplicate admitted")
	}
	f.slots <- struct{}{}
	if f.start(fleetJob{ID: "other"}) {
		t.Fatal("capacity exceeded")
	}
	<-f.slots
	cancel()
	if f.start(fleetJob{ID: "other"}) {
		t.Fatal("admitted after shutdown")
	}
}
