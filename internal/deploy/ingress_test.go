package deploy

import (
	"context"
	"encoding/json"
	"io"
	"net"
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

func TestIngressConfiguration(t *testing.T) {
	m := Manager{Home: t.TempDir()}
	if err := m.ConfigureIngress(Ingress{TrustedProxies: []string{"127.0.0.1/32", "::1/128"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.caddyfile(map[string]State{}); err != nil {
		t.Fatal(err)
	}
	for _, cidr := range []string{"0.0.0.0/0", "::/0", "127.0.0.1\n admin off", "not-a-cidr"} {
		if err := m.ConfigureIngress(Ingress{TrustedProxies: []string{cidr}}); err == nil {
			t.Fatalf("accepted invalid CIDR %q", cidr)
		}
	}
	write(t, filepath.Join(m.Home, "ingress.toml"), "trusted_proxy=['127.0.0.1/32']")
	if _, err := m.caddyfile(map[string]State{}); err == nil {
		t.Fatal("ignored unknown ingress key")
	}
}

// This test uses a real Caddy when available; the suite never downloads it.
func TestCaddyForwardedHeaders(t *testing.T) {
	caddy, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("Caddy is not installed")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(r.Header) }))
	defer upstream.Close()
	for _, tc := range []struct {
		name    string
		ingress Ingress
		proto   string
	}{{"trusted", Ingress{TrustedProxies: []string{"127.0.0.1/32"}}, "https"}, {"untrusted", Ingress{}, "http"}} {
		t.Run(tc.name, func(t *testing.T) {
			states := map[string]State{"api": {Current: Release{Upstream: strings.TrimPrefix(upstream.URL, "http://"), Config: config.Config{Name: "api", Domains: []string{"example.com"}, Docker: &config.Docker{Port: 3000}}}}}
			text, err := caddyfile(states, tc.ingress)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			listener.Close()
			text = strings.Replace(text, "auto_https off", "auto_https off\n admin off", 1)
			text = strings.Replace(text, ":8000 {", address+" {", 1)
			dir := t.TempDir()
			path := filepath.Join(dir, "Caddyfile")
			write(t, path, text)
			log, err := os.Create(filepath.Join(dir, "caddy.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd := exec.Command(caddy, "run", "--config", path, "--adapter", "caddyfile")
			cmd.Stdout = log
			cmd.Stderr = log
			cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				req, err := http.NewRequestWithContext(ctx, "GET", "http://"+address+"/", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "example.com"
				req.Header.Set("X-Forwarded-Proto", "https")
				req.Header.Set("X-Forwarded-For", "198.51.100.25")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					var headers http.Header
					if err := json.Unmarshal(body, &headers); err != nil {
						t.Fatalf("invalid upstream response %s: %v", body, err)
					}
					if got := headers.Get("X-Forwarded-Proto"); got != tc.proto {
						t.Fatalf("scheme = %q, want %q", got, tc.proto)
					}
					if strings.Contains(headers.Get("X-Forwarded-For"), "198.51.100.25") != (tc.proto == "https") {
						t.Fatal("incorrect client IP trust behavior")
					}
					return
				}
				select {
				case <-ctx.Done():
					b, _ := os.ReadFile(log.Name())
					t.Fatalf("Caddy did not respond: %s", b)
				case <-tick.C:
				}
			}
		})
	}
}
