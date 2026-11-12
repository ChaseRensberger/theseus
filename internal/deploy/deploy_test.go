package deploy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"theseus/internal/config"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
}
func TestStaticDeployFailureRollbackAndRecovery(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "caddy"), "#!/bin/sh\nif [ \"$1\" = validate ] && [ \"$FAIL_VALIDATE\" = yes ]; then exit 1; fi\nif [ \"$1\" = reload ] && [ \"$FAIL_RELOAD\" = yes ]; then exit 1; fi\nexit 0\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "theseus.toml"), "name='web'\ndomains=['web.example.com']\n[static]\noutput='.'\n")
	write(t, filepath.Join(repo, "index.html"), "first")
	write(t, filepath.Join(repo, ".env"), "SECRET=no")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "first")
	m := Manager{Home: t.TempDir(), Out: io.Discard}
	ctx := context.Background()
	if err := m.Deploy(ctx, repo, "main"); err != nil {
		t.Fatal(err)
	}
	s, _ := m.States()
	first := s["web"].Current
	for _, name := range []string{".git", ".env", "theseus.toml"} {
		if _, err := os.Stat(filepath.Join(first.Root, name)); !os.IsNotExist(err) {
			t.Fatalf("published %s", name)
		}
	}
	write(t, filepath.Join(repo, "index.html"), "second")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "second")
	t.Setenv("FAIL_VALIDATE", "yes")
	if err := m.Deploy(ctx, repo, "main"); err == nil {
		t.Fatal("expected validation failure")
	}
	s, _ = m.States()
	if s["web"].Current.ID != first.ID {
		t.Fatal("failed deployment changed release")
	}
	t.Setenv("FAIL_VALIDATE", "")
	if err := m.Deploy(ctx, repo, "main"); err != nil {
		t.Fatal(err)
	}
	s, _ = m.States()
	second := s["web"].Current
	if second.Commit == first.Commit {
		t.Fatal("commit did not change")
	}
	if err := m.Rollback(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	s, _ = m.States()
	if s["web"].Current.ID != first.ID {
		t.Fatal("rollback did not restore first release")
	}
	t.Setenv("FAIL_RELOAD", "yes")
	if err := m.Rollback(ctx, "web"); err == nil {
		t.Fatal("expected reload failure")
	}
	if err := m.Deploy(ctx, repo, "main"); err == nil {
		t.Fatal("pending activation did not block deploy")
	}
	t.Setenv("FAIL_RELOAD", "")
	if err := m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ = m.States()
	if s["web"].Current.ID != second.ID {
		t.Fatal("recovery did not complete activation")
	}
}
func TestArtifactBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "secret")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := within(root, "escape"); err == nil {
		t.Fatal("accepted escaping path")
	}
	if err := copyStatic(root, filepath.Join(t.TempDir(), "public")); err == nil {
		t.Fatal("published symlink")
	}
}
func TestDomainCollision(t *testing.T) {
	c := config.Config{Name: "web", Domains: []string{"example.com"}, Static: &config.Static{Output: "."}}
	s := State{Current: Release{Config: c, Root: "/site"}}
	if _, err := Caddyfile(map[string]State{"a": s, "b": s}); err == nil || !strings.Contains(err.Error(), "multiple projects") {
		t.Fatalf("unexpected error: %v", err)
	}
}
func TestDeploymentLock(t *testing.T) {
	m := Manager{Home: t.TempDir()}
	unlock, err := m.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := m.Lock(); err == nil {
		release()
		t.Fatal("second lock acquired")
	}
}

func TestContainerDeploy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "caddy"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\ncase \"$1\" in\nport) printf '%s\\n' \"$TEST_UPSTREAM\" ;;\nbuild|run|rm) exit 0 ;;\n*) exit 1 ;;\nesac\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TEST_UPSTREAM", strings.TrimPrefix(server.URL, "http://"))
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "theseus.toml"), "name='api'\ndomains=['api.example.com']\n[docker]\nport=3000\nhealthcheck='/ready'\n")
	write(t, filepath.Join(repo, "Dockerfile"), "FROM scratch\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "app")
	m := Manager{Home: t.TempDir(), Out: io.Discard}
	if err := m.Deploy(context.Background(), repo, "main"); err != nil {
		t.Fatal(err)
	}
	s, err := m.States()
	if err != nil {
		t.Fatal(err)
	}
	r := s["api"].Current
	if r.Container == "" || r.Upstream != strings.TrimPrefix(server.URL, "http://") {
		t.Fatalf("invalid container release: %+v", r)
	}
	b, err := os.ReadFile(filepath.Join(m.Home, "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "reverse_proxy "+r.Upstream) {
		t.Fatal("missing container route")
	}
}

func TestHealthFailureHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := healthy(ctx, Release{Upstream: strings.TrimPrefix(server.URL, "http://"), Config: config.Config{Docker: &config.Docker{}}})
	if err == nil {
		t.Fatal("unhealthy container accepted")
	}
}
