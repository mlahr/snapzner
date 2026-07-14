package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mlahr/snapzner/internal/config"
	"github.com/mlahr/snapzner/internal/credentials"
	"github.com/mlahr/snapzner/internal/snapzner"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type app struct {
	version    string
	configPath string
	jsonOutput bool
	quiet      bool
	projects   []string
	yes        bool
	out        io.Writer
	errOut     io.Writer
}

func Execute(ctx context.Context, version string) error {
	a := &app{version: version, out: os.Stdout, errOut: os.Stderr}
	root := a.rootCommand()
	root.SetContext(ctx)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "snapzner:", err)
		return err
	}
	return nil
}

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{Use: "snapzner", Short: "Multi-project Hetzner Cloud snapshot backups", SilenceUsage: true, SilenceErrors: true}
	defaultPath, _ := config.DefaultPath()
	root.PersistentFlags().StringVar(&a.configPath, "config", defaultPath, "configuration file")
	root.PersistentFlags().BoolVar(&a.jsonOutput, "json", false, "emit machine-readable JSON")
	root.PersistentFlags().BoolVar(&a.quiet, "quiet", false, "only print errors")
	root.PersistentFlags().StringSliceVar(&a.projects, "project", nil, "project alias to operate on (repeatable)")
	root.PersistentFlags().BoolVarP(&a.yes, "yes", "y", false, "confirm non-interactive mutations")
	root.AddCommand(a.configureCommand(), a.projectsCommand(), a.backupCommand(), a.pruneCommand(), a.snapshotsCommand(), a.replayCommand())
	root.AddCommand(&cobra.Command{Use: "version", Run: func(_ *cobra.Command, _ []string) { fmt.Fprintln(a.out, a.version) }})
	return root
}

func (a *app) configureCommand() *cobra.Command {
	var alias string
	command := &cobra.Command{
		Use: "configure", Short: "Prompt for and securely store a project API token", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if alias == "" {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("--project is required without an interactive terminal")
				}
				fmt.Fprint(a.errOut, "Project alias: ")
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil {
					return err
				}
				alias = strings.TrimSpace(line)
			}
			if alias == "" {
				return fmt.Errorf("project alias cannot be empty")
			}
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("token entry requires an interactive terminal")
			}
			fmt.Fprintf(a.errOut, "Hetzner API token for %s: ", alias)
			secret, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(a.errOut)
			if err != nil {
				return err
			}
			token := strings.TrimSpace(string(secret))
			if token == "" {
				return fmt.Errorf("token cannot be empty")
			}
			validateCtx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			if err := snapzner.NewCloud(token, a.version).Validate(validateCtx); err != nil {
				return fmt.Errorf("token validation failed: %w", err)
			}
			cfg, err := config.LoadOrDefault(a.configPath)
			if err != nil {
				return err
			}
			cfg.UpsertProject(alias)
			if err := config.Save(a.configPath, cfg); err != nil {
				return err
			}
			path := credentials.PathForConfig(a.configPath)
			store, err := credentials.Load(path)
			if err != nil {
				return err
			}
			store.Tokens[alias] = token
			if err := credentials.Save(path, store); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Configured project %s in %s\n", alias, a.configPath)
			return nil
		},
	}
	command.Flags().StringVar(&alias, "project", "", "project alias")
	return command
}

func (a *app) projectsCommand() *cobra.Command {
	root := &cobra.Command{Use: "projects", Short: "Manage configured projects"}
	root.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := config.Load(a.configPath)
		if err != nil {
			return err
		}
		if a.jsonOutput {
			return json.NewEncoder(a.out).Encode(cfg.Projects)
		}
		for _, p := range cfg.Projects {
			fmt.Fprintln(a.out, p.Name)
		}
		return nil
	}})
	root.AddCommand(&cobra.Command{Use: "remove ALIAS", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		if err := a.confirm(fmt.Sprintf("Remove locally stored configuration and credential for project %s?", args[0])); err != nil {
			return err
		}
		cfg, err := config.Load(a.configPath)
		if err != nil {
			return err
		}
		if !cfg.RemoveProject(args[0]) {
			return fmt.Errorf("project %q is not configured", args[0])
		}
		storePath := credentials.PathForConfig(a.configPath)
		store, err := credentials.Load(storePath)
		if err != nil {
			return err
		}
		delete(store.Tokens, args[0])
		if err := config.Save(a.configPath, cfg); err != nil {
			return err
		}
		if err := credentials.Save(storePath, store); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Removed project %s\n", args[0])
		return nil
	}})
	return root
}

func (a *app) backupCommand() *cobra.Command {
	return &cobra.Command{Use: "backup", Short: "Create snapshots and enforce retention", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		unlock, err := a.lock()
		if err != nil {
			return err
		}
		defer unlock()
		return a.runProjects(cmd.Context(), func(ctx context.Context, svc *snapzner.Service, p config.Project) []snapzner.Event {
			return svc.Backup(ctx, p)
		})
	}}
}

func (a *app) pruneCommand() *cobra.Command {
	var apply bool
	command := &cobra.Command{Use: "prune", Short: "Preview or apply snapshot retention", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var unlock func()
		var err error
		if apply {
			unlock, err = a.lock()
			if err != nil {
				return err
			}
			defer unlock()
		}
		return a.runProjects(cmd.Context(), func(ctx context.Context, svc *snapzner.Service, _ config.Project) []snapzner.Event {
			return svc.Prune(ctx, apply)
		})
	}}
	command.Flags().BoolVar(&apply, "apply", false, "delete snapshots instead of previewing")
	return command
}

func (a *app) snapshotsCommand() *cobra.Command {
	root := &cobra.Command{Use: "snapshots", Short: "List and delete snapshots"}
	root.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.runProjects(cmd.Context(), func(ctx context.Context, svc *snapzner.Service, _ config.Project) []snapzner.Event {
			images, err := svc.ListSnapshots(ctx)
			if err != nil {
				return []snapzner.Event{{Project: svc.Project, Operation: "list", Message: "could not list snapshots", Error: err.Error()}}
			}
			events := make([]snapzner.Event, 0, len(images))
			for _, image := range images {
				events = append(events, snapzner.Event{Project: svc.Project, Operation: "list", ResourceID: image.ID, Message: fmt.Sprintf("%s | source=%s | created=%s", image.Description, image.Labels["snapzner.mlahr.dev/source-name"], image.Created.UTC().Format(time.RFC3339))})
			}
			return events
		})
	}})
	var ids []int64
	var force bool
	deleteCommand := &cobra.Command{Use: "delete", Short: "Delete explicitly identified snapshots", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if len(a.projects) != 1 {
			return fmt.Errorf("snapshot deletion requires exactly one --project")
		}
		if len(ids) == 0 {
			return fmt.Errorf("at least one --id is required")
		}
		if err := a.confirm(fmt.Sprintf("Delete snapshot IDs %v from project %s?", ids, a.projects[0])); err != nil {
			return err
		}
		unlock, err := a.lock()
		if err != nil {
			return err
		}
		defer unlock()
		return a.runProjects(cmd.Context(), func(ctx context.Context, svc *snapzner.Service, _ config.Project) []snapzner.Event {
			return svc.DeleteSnapshots(ctx, ids, force)
		})
	}}
	deleteCommand.Flags().Int64SliceVar(&ids, "id", nil, "snapshot ID (repeatable)")
	deleteCommand.Flags().BoolVar(&force, "force-unmanaged", false, "allow deletion of an unmanaged snapshot")
	root.AddCommand(deleteCommand)
	return root
}

func (a *app) replayCommand() *cobra.Command {
	root := &cobra.Command{Use: "replay", Short: "Create or rebuild a server from a snapshot"}
	var clone snapzner.CloneOptions
	var ipv4, ipv6 string
	cloneCmd := &cobra.Command{Use: "clone", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if len(a.projects) != 1 {
			return fmt.Errorf("clone requires exactly one --project")
		}
		if clone.Snapshot == "" {
			return fmt.Errorf("--snapshot is required")
		}
		if err := a.confirm(fmt.Sprintf("Create a billable replay server in project %s from snapshot %s?", a.projects[0], clone.Snapshot)); err != nil {
			return err
		}
		var err error
		clone.EnableIPv4, err = optionalBool(ipv4)
		if err != nil {
			return fmt.Errorf("--ipv4: %w", err)
		}
		clone.EnableIPv6, err = optionalBool(ipv6)
		if err != nil {
			return fmt.Errorf("--ipv6: %w", err)
		}
		unlock, err := a.lock()
		if err != nil {
			return err
		}
		defer unlock()
		return a.runProjects(cmd.Context(), func(ctx context.Context, svc *snapzner.Service, _ config.Project) []snapzner.Event {
			return svc.Clone(ctx, clone)
		})
	}}
	cloneCmd.Flags().StringVar(&clone.Snapshot, "snapshot", "", "snapshot ID or latest")
	cloneCmd.Flags().StringVar(&clone.Source, "source", "", "source server ID or name (required with latest)")
	cloneCmd.Flags().StringVar(&clone.Name, "name", "", "new server name")
	cloneCmd.Flags().StringVar(&clone.ServerType, "server-type", "", "override server type")
	cloneCmd.Flags().StringVar(&clone.Location, "location", "", "override location")
	cloneCmd.Flags().StringVar(&ipv4, "ipv4", "", "override IPv4 enablement (true or false)")
	cloneCmd.Flags().StringVar(&ipv6, "ipv6", "", "override IPv6 enablement (true or false)")
	var snapshot, source, target string
	rebuildCmd := &cobra.Command{Use: "rebuild", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if len(a.projects) != 1 {
			return fmt.Errorf("rebuild requires exactly one --project")
		}
		if snapshot == "" || target == "" {
			return fmt.Errorf("--snapshot and --target are required")
		}
		if err := a.confirm(fmt.Sprintf("Overwrite the root disk of server %s in project %s from snapshot %s?", target, a.projects[0], snapshot)); err != nil {
			return err
		}
		unlock, err := a.lock()
		if err != nil {
			return err
		}
		defer unlock()
		return a.runProjects(cmd.Context(), func(ctx context.Context, svc *snapzner.Service, _ config.Project) []snapzner.Event {
			return svc.Rebuild(ctx, snapshot, source, target)
		})
	}}
	rebuildCmd.Flags().StringVar(&snapshot, "snapshot", "", "snapshot ID or latest")
	rebuildCmd.Flags().StringVar(&source, "source", "", "source server ID or name (required with latest)")
	rebuildCmd.Flags().StringVar(&target, "target", "", "target server ID or name")
	root.AddCommand(cloneCmd, rebuildCmd)
	return root
}

func (a *app) runProjects(ctx context.Context, fn func(context.Context, *snapzner.Service, config.Project) []snapzner.Event) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	store, err := credentials.Load(credentials.PathForConfig(a.configPath))
	if err != nil {
		return err
	}
	projects, err := selectProjects(cfg, a.projects)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, cfg.Defaults.ProjectConcurrency)
	results := make(chan []snapzner.Event, len(projects))
	var wg sync.WaitGroup
	for _, p := range projects {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			token, err := store.Token(p.Name)
			if err != nil {
				results <- []snapzner.Event{{Project: p.Name, Operation: "project", Message: "credential unavailable", Error: err.Error()}}
				return
			}
			svc := &snapzner.Service{Project: p.Name, Cloud: snapzner.NewCloud(token, a.version), Policy: cfg.PolicyFor(p), Timeout: cfg.Defaults.OperationTimeout, ServerConcurrency: cfg.Defaults.ServerConcurrency}
			results <- fn(ctx, svc, p)
		}()
	}
	wg.Wait()
	close(results)
	var events []snapzner.Event
	failed := false
	for batch := range results {
		for _, event := range batch {
			if event.Error != "" {
				failed = true
			}
			events = append(events, event)
		}
	}
	a.printEvents(events)
	if failed {
		return fmt.Errorf("one or more operations failed")
	}
	return nil
}

func selectProjects(cfg config.Config, names []string) ([]config.Project, error) {
	if len(names) == 0 {
		if len(cfg.Projects) == 0 {
			return nil, fmt.Errorf("no projects configured")
		}
		return cfg.Projects, nil
	}
	seen := map[string]bool{}
	var result []config.Project
	for _, name := range names {
		if seen[name] {
			continue
		}
		p, ok := cfg.FindProject(name)
		if !ok {
			return nil, fmt.Errorf("project %q is not configured", name)
		}
		seen[name] = true
		result = append(result, p)
	}
	return result, nil
}

func (a *app) printEvents(events []snapzner.Event) {
	if a.jsonOutput {
		_ = json.NewEncoder(a.out).Encode(events)
		return
	}
	for _, e := range events {
		if a.quiet && e.Error == "" {
			continue
		}
		line := fmt.Sprintf("[%s] %s", e.Project, e.Message)
		if e.ResourceID != 0 {
			line += fmt.Sprintf(" (id=%d)", e.ResourceID)
		}
		if e.Error != "" {
			fmt.Fprintf(a.errOut, "%s: %s\n", line, e.Error)
		} else {
			fmt.Fprintln(a.out, line)
		}
	}
}

func (a *app) confirm(prompt string) error {
	if a.yes {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("confirmation requires an interactive terminal or --yes")
	}
	fmt.Fprintf(a.errOut, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		return fmt.Errorf("operation cancelled")
	}
	return nil
}

func (a *app) lock() (func(), error) {
	path := filepath.Join(filepath.Dir(a.configPath), "snapzner.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another mutating snapzner command is running")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func optionalBool(value string) (*bool, error) {
	if value == "" {
		return nil, nil
	}
	v, err := strconv.ParseBool(value)
	if err != nil {
		return nil, errors.New("must be true or false")
	}
	return &v, nil
}
