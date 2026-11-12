package deploy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"theseus/internal/config"
)

func recoveryManager(t *testing.T) Manager {
	t.Helper()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "caddy"), "#!/bin/sh\nif [ \"$1\" = reload ] && [ \"$FAIL_RELOAD\" = yes ]; then exit 1; fi\nexit 0\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return Manager{Home: t.TempDir(), Out: io.Discard}
}

func TestAbortUnhealthyCandidateAfterStateWrite(t *testing.T) {
	m := recoveryManager(t)
	old := State{Current: Release{ID: "old", Root: t.TempDir(), Config: config.Config{Name: "site", Domains: []string{"example.com"}, Static: &config.Static{}}}, Repository: "owner/site", Sequence: 1}
	before := map[string]State{"site": old}
	if err := m.Activate(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	candidate := State{Current: Release{ID: "candidate", Container: "candidate", Upstream: strings.TrimPrefix(server.URL, "http://"), Config: config.Config{Name: "site", Domains: []string{"example.com"}, Docker: &config.Docker{Port: 3000}}}, Previous: &old.Current, Repository: "owner/site", Sequence: 2}
	after := map[string]State{"site": candidate}
	t.Setenv("FAIL_RELOAD", "yes")
	if err := m.Activate(context.Background(), after); err == nil {
		t.Fatal("expected interrupted activation")
	}
	// Simulate interruption after writing state.json but before completing the
	// transaction. Abort must use the journal's snapshot, not current state.json.
	b, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(m.Home, "state.json"), b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Recover(ctx); err == nil {
		t.Fatal("accepted unhealthy candidate")
	}
	if err := m.Abort(context.Background()); err == nil {
		t.Fatal("expected interrupted abort")
	}
	// A normal recover must resume the selected abort, not retry the candidate.
	t.Setenv("FAIL_RELOAD", "")
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := m.States()
	if err != nil {
		t.Fatal(err)
	}
	if s["site"].Current.ID != "old" || s["site"].Sequence != 1 {
		t.Fatalf("restored wrong state: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending journal: %v", err)
	}
	caddy, err := os.ReadFile(filepath.Join(m.Home, "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(caddy), old.Current.Root) {
		t.Fatal("old routing not restored")
	}
	unlock, err := m.Lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestRecoveryIgnoresUnchangedUnhealthyProject(t *testing.T) {
	m := recoveryManager(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	states := map[string]State{"api": {Current: Release{ID: "unchanged", Container: "api", Upstream: strings.TrimPrefix(server.URL, "http://"), Config: config.Config{Name: "api", Domains: []string{"api.example.com"}, Docker: &config.Docker{Port: 3000}}}}}
	if err := m.Activate(context.Background(), states); err != nil {
		t.Fatal(err)
	}
	states["web"] = State{Current: Release{ID: "new", Root: t.TempDir(), Config: config.Config{Name: "web", Domains: []string{"example.com"}, Static: &config.Static{}}}}
	t.Setenv("FAIL_RELOAD", "yes")
	if err := m.Activate(context.Background(), states); err == nil {
		t.Fatal("expected interrupted activation")
	}
	t.Setenv("FAIL_RELOAD", "")
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("recovery probed an unchanged project")
	}
}

func TestRecoverLegacyJournal(t *testing.T) {
	m := recoveryManager(t)
	// "version" is also a valid project name in the old map-only format.
	states := map[string]State{"version": {Current: Release{ID: "legacy", Root: t.TempDir(), Config: config.Config{Name: "version", Domains: []string{"example.com"}, Static: &config.Static{}}}}}
	data, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(m.Home, "pending.json"), data); err != nil {
		t.Fatal(err)
	}
	if err := m.Abort(context.Background()); err == nil {
		t.Fatal("aborted without a saved previous state")
	}
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := m.States()
	if err != nil || got["version"].Current.ID != "legacy" {
		t.Fatalf("legacy recovery: %v %v", got, err)
	}
}
