package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type notifyWriter struct{ ready chan struct{} }

func (w notifyWriter) Write(p []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestReceiveCancelsBlockedUpload(t *testing.T) {
	var partial bytes.Buffer
	gz := gzip.NewWriter(&partial)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "public/file", Mode: 0644, Size: 1024, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{{"gzip header", nil}, {"tar entry", partial.Bytes()[:10]}, {"file payload", partial.Bytes()}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			ready := make(chan struct{}, 1)
			m := Manager{Home: t.TempDir(), Out: notifyWriter{ready}}
			done := make(chan error, 1)
			go func() {
				done <- m.Receive(ctx, r, Delivery{Repository: "owner/site", Commit: strings.Repeat("a", 40), Branch: "main", Sequence: 1})
			}()
			<-ready
			if len(tc.data) > 0 {
				if _, err := w.Write(tc.data); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upload did not stop")
			}
			unlock, err := m.Lock()
			if err != nil {
				t.Fatal(err)
			}
			unlock()
			logs, err := filepath.Glob(filepath.Join(m.Home, "releases", "*", "build.log"))
			if err != nil || len(logs) != 1 {
				t.Fatalf("logs: %v %v", logs, err)
			}
			b, err := os.ReadFile(logs[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "Deployment failed: context canceled") {
				t.Fatalf("missing cancellation outcome: %s", b)
			}
		})
	}
}

func TestReceiveLogsConfigurationFailure(t *testing.T) {
	m := Manager{Home: t.TempDir(), Out: io.Discard}
	data := bundle(t, map[string]string{"theseus.toml": "name='web'\ndomains=['example.com']\n[static]\nunknown=true"})
	err := m.Receive(context.Background(), bytes.NewReader(data), Delivery{Repository: "owner/site", Commit: strings.Repeat("a", 40), Branch: "main", Sequence: 1})
	if err == nil {
		t.Fatal("expected config error")
	}
	logs, _ := filepath.Glob(filepath.Join(m.Home, "releases", "*", "build.log"))
	if len(logs) != 1 {
		t.Fatal("missing release log")
	}
	b, readErr := os.ReadFile(logs[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(b), "Deployment failed: "+err.Error()) {
		t.Fatalf("missing failure: %s", b)
	}
}
