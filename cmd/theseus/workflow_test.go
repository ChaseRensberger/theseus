package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowSSHTransport(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("ssh", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_ARGS\"\ncat > \"$TEST_PAYLOAD\"\n")
	write("bundle.tar.gz", "artifact bytes")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TEST_ARGS", filepath.Join(dir, "args"))
	t.Setenv("TEST_PAYLOAD", filepath.Join(dir, "payload"))
	t.Setenv("THESEUS_SSH_KEY", "test key")
	t.Setenv("THESEUS_KNOWN_HOSTS", "test known host")
	t.Setenv("THESEUS_SSH_DEST", "vm+test@vm.exe.xyz")
	t.Setenv("GITHUB_REPOSITORY", "owner/site")
	t.Setenv("GITHUB_SHA", strings.Repeat("a", 40))
	t.Setenv("GITHUB_RUN_NUMBER", "42")
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv("THESEUS_IMAGE", "")
	run := func() error {
		cmd := exec.Command("bash", "../../examples/github-actions/theseus-deploy.sh", filepath.Join(dir, "bundle.tar.gz"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(out))
		}
		return err
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"StrictHostKeyChecking=yes", "IdentitiesOnly=yes", "vm+test@vm.exe.xyz", "theseus receive", "--repository 'owner/site'", "--sequence '42'"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in SSH args: %s", want, b)
		}
	}
	b, err = os.ReadFile(filepath.Join(dir, "payload"))
	if err != nil || string(b) != "artifact bytes" {
		t.Fatalf("payload: %s, %v", b, err)
	}
	t.Setenv("GITHUB_SHA", "'; touch /tmp/not-allowed; '")
	if err := run(); err == nil {
		t.Fatal("accepted shell characters in metadata")
	}
}
