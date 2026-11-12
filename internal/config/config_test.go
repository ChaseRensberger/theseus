package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"static", "name='web'\ndomains=['web.example.com']\n[static]\noutput='public'", true},
		{"unknown", "name='web'\ndomains=['web.example.com']\n[static]\noutpt='public'", false},
		{"ambiguous", "name='web'\ndomains=['web.example.com']\n[static]\n[docker]\nport=80", false},
		{"injection", "name='web'\ndomains=['x { respond 200 }']\n[static]", false},
		{"missing image", "name='web'\ndomains=['web.example.com']\n[static]\nbuild='make'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "theseus.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("Load() error = %v", err)
			}
			if tc.valid && c.Branch != "main" {
				t.Fatalf("branch = %q", c.Branch)
			}
		})
	}
}
