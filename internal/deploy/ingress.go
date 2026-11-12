package deploy

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Ingress is VM-level configuration, independent of application repositories.
type Ingress struct {
	TrustedProxies []string `toml:"trusted_proxies"`
}

func InitialCaddyfile(i Ingress) (string, error) {
	return caddyfile(map[string]State{}, i)
}

func (i Ingress) Validate() error {
	for _, value := range i.TrustedProxies {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Bits() == 0 {
			return fmt.Errorf("trusted proxy %q must be a specific IP CIDR, not a catch-all range", value)
		}
	}
	return nil
}

func (m Manager) ConfigureIngress(i Ingress) error {
	if err := i.Validate(); err != nil {
		return err
	}
	var data bytes.Buffer
	if err := toml.NewEncoder(&data).Encode(i); err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(m.Home, "ingress.toml"), data.Bytes())
}

func (m Manager) caddyfile(states map[string]State) (string, error) {
	var ingress Ingress
	data, err := os.ReadFile(filepath.Join(m.Home, "ingress.toml"))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		meta, err := toml.Decode(string(data), &ingress)
		if err != nil {
			return "", err
		}
		if keys := meta.Undecoded(); len(keys) > 0 {
			return "", fmt.Errorf("unknown ingress settings: %v", keys)
		}
	}
	return caddyfile(states, ingress)
}

func ingressOptions(i Ingress) (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	if len(i.TrustedProxies) == 0 {
		return "", nil
	}
	return " servers {\n  trusted_proxies static " + strings.Join(i.TrustedProxies, " ") + "\n  trusted_proxies_strict\n }\n", nil
}
