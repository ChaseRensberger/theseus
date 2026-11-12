package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"theseus/internal/config"
)

type Release struct {
	ID         string        `json:"id"`
	Commit     string        `json:"commit"`
	Config     config.Config `json:"config"`
	Root       string        `json:"root,omitempty"`
	Container  string        `json:"container,omitempty"`
	Upstream   string        `json:"upstream,omitempty"`
	Repository string        `json:"repository,omitempty"`
	Sequence   uint64        `json:"sequence,omitempty"`
	Image      string        `json:"image,omitempty"`
}
type State struct {
	Current    Release  `json:"current"`
	Previous   *Release `json:"previous,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Sequence   uint64   `json:"sequence,omitempty"`
}
type Manager struct {
	Home string
	Out  io.Writer
}

func (m Manager) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var output strings.Builder
	cmd.Stdout = io.MultiWriter(&output, m.Out)
	cmd.Stderr = m.Out
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("%s: %w", args[0], err)
	}
	return strings.TrimSpace(output.String()), nil
}
func (m Manager) Lock() (func(), error) {
	if err := os.MkdirAll(m.Home, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.Home, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another deployment is running")
	}
	return func() { f.Close() }, nil
}
func (m Manager) States() (map[string]State, error) {
	states := map[string]State{}
	b, err := os.ReadFile(filepath.Join(m.Home, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return states, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &states)
	return states, err
}
func AtomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".theseus-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func Caddyfile(states map[string]State) (string, error) {
	return caddyfile(states, Ingress{})
}

func caddyfile(states map[string]State, ingress Ingress) (string, error) {
	var b strings.Builder
	options, err := ingressOptions(ingress)
	if err != nil {
		return "", err
	}
	b.WriteString("{\n auto_https off\n" + options + "}\n:8000 {\n")
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for i, name := range names {
		r := states[name].Current
		if err := r.Config.Validate(); err != nil {
			return "", err
		}
		for _, domain := range r.Config.Domains {
			domain = strings.ToLower(domain)
			if seen[domain] {
				return "", fmt.Errorf("domain %s belongs to multiple projects", domain)
			}
			seen[domain] = true
		}
		fmt.Fprintf(&b, " @app%d host %s\n handle @app%d {\n", i, strings.Join(r.Config.Domains, " "), i)
		if r.Config.Static != nil {
			fmt.Fprintf(&b, "  root * %q\n  file_server\n", r.Root)
		} else {
			fmt.Fprintf(&b, "  reverse_proxy %s\n", r.Upstream)
		}
		b.WriteString(" }\n")
	}
	b.WriteString(" handle {\n  respond \"Not found\" 404\n }\n}\n")
	return b.String(), nil
}
func (m Manager) Activate(ctx context.Context, states map[string]State) error {
	before, err := m.States()
	if err != nil {
		return err
	}
	return m.apply(ctx, transaction{Version: 1, Before: before, After: states})
}

type transaction struct {
	Version int              `json:"version"`
	Before  map[string]State `json:"before"`
	After   map[string]State `json:"after"`
	Abort   bool             `json:"abort"`
}

func (m Manager) apply(ctx context.Context, tx transaction) error {
	states := tx.After
	if tx.Abort {
		states = tx.Before
	}
	text, err := m.caddyfile(states)
	if err != nil {
		return err
	}
	path := filepath.Join(m.Home, "Caddyfile.next")
	if err = AtomicWrite(path, []byte(text)); err != nil {
		return err
	}
	if _, err = m.Run(ctx, "caddy", "validate", "--config", path, "--adapter", "caddyfile"); err != nil {
		return err
	}
	data, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return err
	}
	// Persist intent before switching traffic so interruption is recoverable.
	journal, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	if err = AtomicWrite(filepath.Join(m.Home, "pending.json"), journal); err != nil {
		return err
	}
	if _, err = m.Run(ctx, "caddy", "reload", "--config", path, "--adapter", "caddyfile"); err != nil {
		return fmt.Errorf("reload incomplete; run recover after fixing Caddy: %w", err)
	}
	if err = AtomicWrite(filepath.Join(m.Home, "state.json"), data); err != nil {
		return fmt.Errorf("routing changed; run recover: %w", err)
	}
	if err = AtomicWrite(filepath.Join(m.Home, "Caddyfile"), []byte(text)); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(m.Home, "pending.json")); err != nil {
		return err
	}
	dir, err := os.Open(m.Home)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func within(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("path must be relative: %s", relative)
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path escapes checkout: %s", relative)
	}
	return p, nil
}
func (m Manager) Deploy(ctx context.Context, repo, branch string) error {
	return m.deploy(ctx, repo, branch, nil, nil)
}

func (m Manager) deploy(ctx context.Context, repo, branch string, artifact io.Reader, ci *Delivery) (result error) {
	unlock, err := m.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(m.Home, "pending.json")); err == nil {
		return fmt.Errorf("unfinished activation: run recover first")
	}
	states, err := m.States()
	if err != nil {
		return err
	}
	id := time.Now().UTC().Format("20060102T150405.000000000")
	dir := filepath.Join(m.Home, "releases", id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(dir, "build.log"))
	if err != nil {
		return err
	}
	m.Out = io.MultiWriter(m.Out, log)
	defer func() {
		if result != nil {
			fmt.Fprintf(m.Out, "Deployment failed: %v\n", result)
		}
		log.Close()
	}()
	fmt.Fprintf(m.Out, "Preparing release %s\n", id)
	source := filepath.Join(dir, "source")
	if ci != nil {
		if err = extract(ctx, artifact, source); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	} else {
		if _, err = m.Run(ctx, "git", "clone", "--depth", "1", "--branch", branch, "--", repo, source); err != nil {
			return err
		}
	}
	c, err := config.Load(filepath.Join(source, "theseus.toml"))
	if err != nil {
		return err
	}
	if c.Branch != branch {
		return fmt.Errorf("configured branch %q differs from selected branch %q", c.Branch, branch)
	}
	var commit string
	if ci != nil {
		commit = ci.Commit
		old := states[c.Name]
		if old.Repository != "" && old.Repository != ci.Repository {
			return fmt.Errorf("project belongs to repository %s", old.Repository)
		}
		if ci.Sequence <= old.Sequence {
			return fmt.Errorf("stale delivery: sequence %d is not newer than %d", ci.Sequence, old.Sequence)
		}
		if (c.Static != nil) != (ci.Image == "") {
			return fmt.Errorf("--image is required only for [docker] projects")
		}
	} else {
		if states[c.Name].Repository != "" {
			return fmt.Errorf("project is managed by CI; use receive or rollback")
		}
		commit, err = m.Run(ctx, "git", "-C", source, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
	}
	r := Release{ID: id, Commit: commit, Config: c}
	if ci != nil {
		r.Repository = ci.Repository
		r.Sequence = ci.Sequence
		r.Image = ci.Image
	}
	activated := false
	defer func() {
		if activated || r.Container == "" {
			return
		}
		// Keep a candidate if routing might already point to it.
		if _, err := os.Stat(filepath.Join(m.Home, "pending.json")); !os.IsNotExist(err) {
			return
		}
		committed, err := m.States()
		if err != nil || committed[c.Name].Current.ID == r.ID {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := m.Run(cleanup, "docker", "rm", "--force", r.Container); err != nil {
			fmt.Fprintf(m.Out, "Candidate cleanup failed: %v\n", err)
		}
	}()
	if c.Static != nil {
		if ci == nil && c.Static.Build != "" {
			_, err = m.Run(ctx, "docker", "run", "--rm", "--memory", "1g", "--cpus", "1", "--mount", "type=bind,src="+source+",dst=/workspace", "--workdir", "/workspace", c.Static.Image, "sh", "-c", c.Static.Build)
			if err != nil {
				return err
			}
		}
		output := c.Static.Output
		if ci != nil {
			output = "public"
		}
		r.Root, err = within(source, output)
		if err != nil {
			return err
		}
		// Publish only artifacts, never the repository metadata or config.
		root := filepath.Join(dir, "public")
		if err = copyStatic(r.Root, root); err != nil {
			return err
		}
		r.Root = root
	} else {
		image := "theseus/" + c.Name + ":" + id
		if ci != nil {
			image = ci.Image
			if _, err = m.Run(ctx, "docker", "pull", image); err != nil {
				return err
			}
		} else {
			file, err := within(source, c.Docker.Dockerfile)
			if err != nil {
				return err
			}
			if _, err = m.Run(ctx, "docker", "build", "--tag", image, "--file", file, source); err != nil {
				return err
			}
		}
		r.Container = "theseus-" + c.Name + "-" + strings.ReplaceAll(id, ".", "-")
		if _, err = m.Run(ctx, "docker", "run", "--detach", "--restart", "unless-stopped", "--name", r.Container, "--publish", fmt.Sprintf("127.0.0.1::%d", c.Docker.Port), image); err != nil {
			return err
		}
		port, err := m.Run(ctx, "docker", "port", r.Container, fmt.Sprintf("%d/tcp", c.Docker.Port))
		if err != nil {
			return err
		}
		r.Upstream = port
		if err = healthy(ctx, r); err != nil {
			return err
		}
	}
	state := State{Current: r, Repository: r.Repository, Sequence: r.Sequence}
	if old, ok := states[c.Name]; ok {
		state.Previous = &old.Current
	}
	states[c.Name] = state
	if err = m.Activate(ctx, states); err != nil {
		return err
	}
	activated = true
	fmt.Fprintf(m.Out, "Deployed %s at %s (%s)\n", c.Name, id, commit)
	return nil
}
func copyStatic(source, dest string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.Name() == ".git" || entry.Name() == "theseus.toml" || strings.HasPrefix(entry.Name(), ".env") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("static artifact contains symlink: %s", rel)
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported static artifact: %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(filepath.Join(dest, rel), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
func healthy(ctx context.Context, r Release) error {
	path := r.Config.Docker.Healthcheck
	if path == "" {
		path = "/"
	}
	client := http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", "http://"+r.Upstream+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("health check failed for %s", r.Container)
		case <-tick.C:
		}
	}
}
func (m Manager) Rollback(ctx context.Context, name string) error {
	unlock, err := m.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(m.Home, "pending.json")); err == nil {
		return fmt.Errorf("run recover first")
	}
	states, err := m.States()
	if err != nil {
		return err
	}
	s, ok := states[name]
	if !ok || s.Previous == nil {
		return fmt.Errorf("no previous release for %s", name)
	}
	if s.Previous.Container != "" {
		if err = healthy(ctx, *s.Previous); err != nil {
			return err
		}
	}
	states[name] = State{Current: *s.Previous, Previous: &s.Current, Repository: s.Repository, Sequence: s.Sequence}
	return m.Activate(ctx, states)
}
func (m Manager) Recover(ctx context.Context) error {
	return m.recover(ctx, false)
}

func (m Manager) Abort(ctx context.Context) error {
	return m.recover(ctx, true)
}

func (m Manager) recover(ctx context.Context, abort bool) error {
	unlock, err := m.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, err := os.ReadFile(filepath.Join(m.Home, "pending.json"))
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil {
		return err
	}
	var tx transaction
	// Legacy journals contain only the target state. They can be completed,
	// but do not reliably preserve the pre-deployment state for an abort.
	var version int
	versioned := json.Unmarshal(fields["version"], &version) == nil
	if !versioned {
		if abort {
			return fmt.Errorf("legacy journal has no saved previous state; complete recovery before rollback")
		}
		if err = json.Unmarshal(b, &tx.After); err != nil {
			return err
		}
		tx.Version = 1
	} else if err = json.Unmarshal(b, &tx); err != nil {
		return err
	}
	if tx.Version != 1 || tx.After == nil {
		return fmt.Errorf("invalid activation journal")
	}
	if (abort || tx.Abort) && tx.Before == nil {
		return fmt.Errorf("activation journal has no saved previous state")
	}
	tx.Abort = tx.Abort || abort
	if !tx.Abort {
		for name, s := range tx.After {
			if s.Current.Container != "" && s.Current.ID != tx.Before[name].Current.ID {
				if err := healthy(ctx, s.Current); err != nil {
					return fmt.Errorf("%w; use recover --abort to restore the pre-deployment routing", err)
				}
			}
		}
	}
	return m.apply(ctx, tx)
}
