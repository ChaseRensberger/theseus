package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReceiveStaticOrderingAndRollback(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "caddy"), "#!/bin/sh\nexit 0\n")
	// Any VM-side build or source fetch is a test failure.
	for _, name := range []string{"git", "docker"} {
		write(t, filepath.Join(bin, name), "#!/bin/sh\nexit 99\n")
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	data := bundle(t, map[string]string{"theseus.toml": "name='web'\ndomains=['example.com']\n[static]\n", "public/index.html": "built in CI"})
	m := Manager{Home: t.TempDir(), Out: io.Discard}
	d := Delivery{Repository: "owner/site", Commit: strings.Repeat("a", 40), Branch: "main", Sequence: 1}
	receive := func() error { return m.Receive(context.Background(), bytes.NewReader(data), d) }
	if err := receive(); err != nil {
		t.Fatal(err)
	}
	s, _ := m.States()
	first := s["web"].Current.ID
	d.Sequence = 3
	d.Commit = strings.Repeat("b", 40)
	if err := receive(); err != nil {
		t.Fatal(err)
	}
	if err := m.Rollback(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	s, _ = m.States()
	if s["web"].Current.ID != first || s["web"].Sequence != 3 {
		t.Fatal("rollback lost sequence watermark")
	}
	for _, seq := range []uint64{2, 3} {
		d.Sequence = seq
		if err := receive(); err == nil {
			t.Fatal("accepted stale or replayed release")
		}
	}
	d.Sequence = 4
	d.Repository = "other/repo"
	if err := receive(); err == nil {
		t.Fatal("accepted another repository")
	}
	d.Repository = "owner/site"
	d.Branch = "feature"
	if err := receive(); err == nil {
		t.Fatal("accepted wrong branch")
	}
	d.Branch = "main"
	if err := receive(); err != nil {
		t.Fatal(err)
	}
	s, _ = m.States()
	b, err := os.ReadFile(filepath.Join(s["web"].Current.Root, "index.html"))
	if err != nil || string(b) != "built in CI" {
		t.Fatalf("artifact: %s %v", b, err)
	}
}

func TestReceiveContainerPullsDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	write(t, filepath.Join(bin, "caddy"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\ncase \"$1\" in\nport) printf '%s\\n' \"$UPSTREAM\" ;;\npull|run|rm) exit 0 ;;\n*) exit 99 ;;\nesac\n")
	write(t, filepath.Join(bin, "git"), "#!/bin/sh\nexit 99\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("CALLS", calls)
	t.Setenv("UPSTREAM", strings.TrimPrefix(server.URL, "http://"))
	d := Delivery{Repository: "owner/api", Commit: strings.Repeat("a", 40), Branch: "main", Sequence: 1, Image: "ghcr.io/owner/api@sha256:" + strings.Repeat("b", 64)}
	data := bundle(t, map[string]string{"theseus.toml": "name='api'\ndomains=['api.example.com']\n[docker]\nport=3000\n"})
	m := Manager{Home: t.TempDir(), Out: io.Discard}
	if err := m.Receive(context.Background(), bytes.NewReader(data), d); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "pull "+d.Image) {
		t.Fatal("digest was not pulled")
	}
	s, _ := m.States()
	if s["api"].Current.Image != d.Image {
		t.Fatal("digest not recorded")
	}
	d.Image = "ghcr.io/owner/api:latest"
	if err := m.Receive(context.Background(), bytes.NewReader(data), d); err == nil {
		t.Fatal("accepted mutable tag")
	}
}

func TestArchiveBoundaries(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "public/../../escape", "public/../escape", "other", "public\\escape"} {
		t.Run(name, func(t *testing.T) {
			if err := extract(context.Background(), bytes.NewReader(bundle(t, map[string]string{name: "bad"})), t.TempDir()); err == nil {
				t.Fatal("accepted invalid archive")
			}
		})
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo} {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "public/link", Typeflag: kind, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()
		if err := extract(context.Background(), &b, t.TempDir()); err == nil {
			t.Fatal("accepted special entry")
		}
	}
	data := bundle(t, map[string]string{"theseus.toml": "hello"})
	data[len(data)-8] ^= 0xff
	if err := extract(context.Background(), bytes.NewReader(data), t.TempDir()); err == nil {
		t.Fatal("accepted corrupt gzip checksum")
	}
}
