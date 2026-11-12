package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"theseus/internal/config"
	"theseus/internal/deploy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func command() *cobra.Command {
	base, _ := os.UserHomeDir()
	home := filepath.Join(base, ".local", "share", "theseus")
	root := &cobra.Command{Use: "theseus", Short: "Git-driven deployments on one VM", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&home, "home", home, "deployment state directory")
	manager := func() deploy.Manager {
		absolute, _ := filepath.Abs(home)
		return deploy.Manager{Home: absolute, Out: os.Stderr}
	}
	var trustedProxies []string
	initCmd := &cobra.Command{Use: "init", Short: "Initialize local state and a Caddy configuration (installs nothing)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		m := manager()
		unlock, err := m.Lock()
		if err != nil {
			return err
		}
		defer unlock()
		path := filepath.Join(m.Home, "Caddyfile")
		if _, err = os.Stat(path); err == nil {
			return fmt.Errorf("already initialized: %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
		ingress := deploy.Ingress{TrustedProxies: trustedProxies}
		if err := ingress.Validate(); err != nil {
			return err
		}
		text, err := deploy.InitialCaddyfile(ingress)
		if err != nil {
			return err
		}
		if err = m.ConfigureIngress(ingress); err != nil {
			return err
		}
		if err = deploy.AtomicWrite(path, []byte(text)); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Initialized %s\nStart Caddy as the same user: caddy run --config %q --adapter caddyfile\n", m.Home, path)
		return nil
	}}
	initCmd.Flags().StringSliceVar(&trustedProxies, "trusted-proxy", nil, "trusted ingress IP CIDRs (repeatable); omit for direct HTTP")
	root.AddCommand(initCmd)
	root.AddCommand(&cobra.Command{Use: "validate [theseus.toml]", Short: "Validate a project configuration", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path := "theseus.toml"
		if len(args) > 0 {
			path = args[0]
		}
		c, err := config.Load(path)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), lipgloss.NewStyle().Bold(true).Render(c.Name)+": valid")
		return nil
	}})
	branch := "main"
	d := &cobra.Command{Use: "deploy <repository>", Short: "Clone and deploy a Git URL or local repository", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
		defer cancel()
		return manager().Deploy(ctx, args[0], branch)
	}}
	d.Flags().StringVar(&branch, "branch", "main", "branch to deploy")
	root.AddCommand(d)
	var delivery deploy.Delivery
	var artifact string
	receive := &cobra.Command{Use: "receive", Short: "Deploy a prebuilt tar.gz bundle from CI", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		var input io.Reader = cmd.InOrStdin()
		if artifact != "-" {
			f, err := os.Open(artifact)
			if err != nil {
				return err
			}
			defer f.Close()
			input = f
		}
		// Inherited stdin may be a blocking descriptor outside Go's network
		// poller. Duplicate it in nonblocking mode so Close interrupts reads.
		if f, ok := input.(*os.File); ok {
			pollable, err := cancellableFile(f)
			if err != nil {
				return err
			}
			defer pollable.Close()
			input = pollable
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
		defer cancel()
		return manager().Receive(ctx, input, delivery)
	}}
	receive.Flags().StringVar(&artifact, "artifact", "-", "tar.gz bundle path, or - for stdin")
	receive.Flags().StringVar(&delivery.Repository, "repository", "", "GitHub owner/repository")
	receive.Flags().StringVar(&delivery.Commit, "commit", "", "full Git commit SHA")
	receive.Flags().StringVar(&delivery.Branch, "branch", "main", "source branch")
	receive.Flags().Uint64Var(&delivery.Sequence, "sequence", 0, "monotonically increasing workflow run number")
	receive.Flags().StringVar(&delivery.Image, "image", "", "container image pinned by sha256 digest")
	root.AddCommand(receive)
	root.AddCommand(&cobra.Command{Use: "status", Short: "Print current and previous releases as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		s, err := manager().States()
		if err != nil {
			return err
		}
		e := json.NewEncoder(cmd.OutOrStdout())
		e.SetIndent("", "  ")
		return e.Encode(s)
	}})
	root.AddCommand(&cobra.Command{Use: "rollback <project>", Short: "Switch to the previous release", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return manager().Rollback(cmd.Context(), args[0]) }})
	var abort bool
	recoverCmd := &cobra.Command{Use: "recover", Short: "Finish an interrupted activation, or restore its previous routing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if abort {
			return manager().Abort(cmd.Context())
		}
		return manager().Recover(cmd.Context())
	}}
	recoverCmd.Flags().BoolVar(&abort, "abort", false, "restore the saved pre-deployment routing")
	root.AddCommand(recoverCmd)
	root.AddCommand(&cobra.Command{Use: "logs <release-id>", Short: "Print a release's build log, including failed builds", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if filepath.Base(args[0]) != args[0] || args[0] == "." || args[0] == ".." {
			return fmt.Errorf("invalid release ID")
		}
		f, err := os.Open(filepath.Join(manager().Home, "releases", args[0], "build.log"))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(cmd.OutOrStdout(), f)
		return err
	}})
	return root
}

func cancellableFile(f *os.File) (*os.File, error) {
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}
