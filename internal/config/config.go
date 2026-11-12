package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Name    string   `toml:"name"`
	Branch  string   `toml:"branch"`
	Domains []string `toml:"domains"`
	Static  *Static  `toml:"static"`
	Docker  *Docker  `toml:"docker"`
}
type Static struct {
	Image  string `toml:"image"`
	Build  string `toml:"build"`
	Output string `toml:"output"`
}
type Docker struct {
	Dockerfile  string `toml:"dockerfile"`
	Port        int    `toml:"port"`
	Healthcheck string `toml:"healthcheck"`
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var domainPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	meta, err := toml.Decode(string(b), &c)
	if err != nil {
		return c, err
	}
	if keys := meta.Undecoded(); len(keys) > 0 {
		return c, fmt.Errorf("unknown configuration keys: %v", keys)
	}
	if c.Branch == "" {
		c.Branch = "main"
	}
	if c.Static != nil && c.Static.Output == "" {
		c.Static.Output = "."
	}
	if c.Docker != nil && c.Docker.Dockerfile == "" {
		c.Docker.Dockerfile = "Dockerfile"
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if !namePattern.MatchString(c.Name) {
		return fmt.Errorf("name must be lowercase letters, digits and hyphens, starting with a letter")
	}
	if (c.Static == nil) == (c.Docker == nil) {
		return fmt.Errorf("configure exactly one of [static] or [docker]")
	}
	if len(c.Domains) == 0 {
		return fmt.Errorf("at least one domain is required")
	}
	for _, d := range c.Domains {
		if !domainPattern.MatchString(d) || strings.Contains(d, "..") {
			return fmt.Errorf("invalid domain %q", d)
		}
	}
	if c.Static != nil && c.Static.Build != "" && c.Static.Image == "" {
		return fmt.Errorf("static.image is required when static.build is set")
	}
	if c.Docker != nil && (c.Docker.Port < 1 || c.Docker.Port > 65535) {
		return fmt.Errorf("docker.port must be between 1 and 65535")
	}
	if c.Docker != nil && c.Docker.Healthcheck != "" && !strings.HasPrefix(c.Docker.Healthcheck, "/") {
		return fmt.Errorf("docker.healthcheck must start with /")
	}
	return nil
}
