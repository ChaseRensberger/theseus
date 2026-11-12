package deploy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Delivery identifies a build from a single CI workflow per project.
type Delivery struct {
	Repository string
	Commit     string
	Branch     string
	Sequence   uint64
	Image      string
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)

func (m Manager) Receive(ctx context.Context, artifact io.Reader, d Delivery) error {
	if !repositoryPattern.MatchString(d.Repository) || !commitPattern.MatchString(d.Commit) || d.Sequence == 0 || d.Branch == "" {
		return fmt.Errorf("repository (owner/name), 40-character commit SHA, branch and positive sequence are required")
	}
	if d.Image != "" && !imagePattern.MatchString(d.Image) {
		return fmt.Errorf("image must be pinned as registry/path@sha256:<64 lowercase hex characters>")
	}
	// Transport readers must be closable so cancellation interrupts a blocked
	// gzip header, tar entry, or payload read. In-memory readers need no closer.
	if closer, ok := artifact.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { closer.Close() })
		defer stop()
	}
	return m.deploy(ctx, "", d.Branch, artifact, &d)
}

// Archives contain only theseus.toml and public/. No links or special files.
func extract(ctx context.Context, input io.Reader, dest string) error {
	const limit int64 = 1 << 30
	gz, err := gzip.NewReader(io.LimitReader(input, limit+1))
	if err != nil {
		return err
	}
	defer gz.Close()
	bounded := &io.LimitedReader{R: gz, N: limit + 1}
	tr := tar.NewReader(bounded)
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if count >= 100000 {
			return fmt.Errorf("archive has too many entries")
		}
		name := strings.TrimPrefix(h.Name, "./")
		name = strings.TrimSuffix(name, "/")
		if name == "" || path.Clean(name) != name || strings.Contains(name, "\\") || (name != "theseus.toml" && name != "public" && !strings.HasPrefix(name, "public/")) {
			return fmt.Errorf("invalid archive path %q", h.Name)
		}
		if h.Size < 0 || h.Size > limit {
			return fmt.Errorf("archive entry too large")
		}
		p := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if name == "theseus.toml" {
				return fmt.Errorf("theseus.toml must be a file")
			}
			if err := os.MkdirAll(p, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if name == "public" {
				return fmt.Errorf("public must be a directory")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("archive links and special files are unsupported: %s", name)
		}
	}
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return err
	}
	if bounded.N <= 0 {
		return fmt.Errorf("expanded archive exceeds 1 GiB")
	}
	return nil
}
