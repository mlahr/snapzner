package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/mlahr/snapzner/internal/config"
	"github.com/mlahr/snapzner/internal/credentials"
	"github.com/mlahr/snapzner/internal/filetxn"
	"github.com/mlahr/snapzner/internal/snapzner"
	"golang.org/x/term"
)

type wizardPrompter interface {
	Line(label, current string) (string, error)
	Raw(label string) (string, error)
	Secret(label string) (string, error)
	Confirm(label string, defaultYes bool) (bool, error)
}

type terminalPrompter struct {
	ctx    context.Context
	in     *os.File
	out    io.Writer
	reader *bufio.Reader
}

func newTerminalPrompter(ctx context.Context, in *os.File, out io.Writer) *terminalPrompter {
	return &terminalPrompter{ctx: ctx, in: in, out: out, reader: bufio.NewReader(in)}
}

func (p *terminalPrompter) Line(label, current string) (string, error) {
	display := current
	if display == "" {
		display = "<disabled>"
	}
	fmt.Fprintf(p.out, "%s [%s]: ", label, display)
	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return current, nil
	}
	return line, nil
}

func (p *terminalPrompter) Raw(label string) (string, error) {
	fmt.Fprint(p.out, label)
	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (p *terminalPrompter) Secret(label string) (string, error) {
	fmt.Fprint(p.out, label)
	result := make(chan struct {
		value []byte
		err   error
	}, 1)
	go func() {
		value, err := term.ReadPassword(int(p.in.Fd()))
		result <- struct {
			value []byte
			err   error
		}{value, err}
	}()
	var secret []byte
	var err error
	select {
	case got := <-result:
		secret, err = got.value, got.err
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	}
	fmt.Fprintln(p.out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(secret)), nil
}

func (p *terminalPrompter) readLine() (string, error) {
	result := make(chan struct {
		value string
		err   error
	}, 1)
	go func() {
		value, err := p.reader.ReadString('\n')
		result <- struct {
			value string
			err   error
		}{value, err}
	}()
	select {
	case got := <-result:
		return got.value, got.err
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	}
}

func (p *terminalPrompter) Confirm(label string, defaultYes bool) (bool, error) {
	suffix := " [y/N]: "
	if defaultYes {
		suffix = " [Y/n]: "
	}
	for {
		answer, err := p.Raw(label + suffix)
		if err != nil {
			return false, err
		}
		if answer == "" {
			return defaultYes, nil
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.out, "Please answer yes or no.")
		}
	}
}

type projectAPI interface {
	Validate(context.Context) error
	AllServers(context.Context) ([]*hcloud.Server, error)
	SelectorServers(context.Context, string) ([]*hcloud.Server, error)
}

type configureWizard struct {
	ctx        context.Context
	prompt     wizardPrompter
	out        io.Writer
	factory    func(string) projectAPI
	picker     func(context.Context, io.Writer, string, pickerState) (map[int64]bool, error)
	configPath string
	version    string
}

type projectSummary struct {
	name, tokenState string
	selected, total  int
}

func (a *app) runConfigure(ctx context.Context) error {
	if len(a.projects) != 0 {
		return fmt.Errorf("configure edits all projects; do not pass --project")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("configure requires an interactive terminal")
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	w := configureWizard{
		ctx: ctx, prompt: newTerminalPrompter(ctx, os.Stdin, a.errOut), out: a.errOut,
		factory:    func(token string) projectAPI { return snapzner.NewCloud(token, a.version) },
		picker:     runServerPicker,
		configPath: a.configPath, version: a.version,
	}
	return w.run(a.out)
}

func (w *configureWizard) run(successOut io.Writer) error {
	cfg, err := config.LoadOrDefault(w.configPath)
	if err != nil {
		return err
	}
	credentialPath := credentials.PathForConfig(w.configPath)
	stored, err := credentials.Load(credentialPath)
	if err != nil {
		return err
	}

	fmt.Fprintln(w.out, "Snapzner configuration")
	fmt.Fprintln(w.out, "Press Enter to keep the displayed value.")
	if err := w.configureDefaults(&cfg.Defaults); err != nil {
		return err
	}

	finalStore := credentials.Store{Version: 1, Tokens: map[string]string{}}
	var projects []config.Project
	var summaries []projectSummary
	seen := map[string]bool{}
	for _, existing := range cfg.Projects {
		keep, err := w.prompt.Confirm(fmt.Sprintf("Keep project %s?", existing.Name), true)
		if err != nil {
			return err
		}
		if !keep {
			continue
		}
		project, token, summary, err := w.configureProject(existing, stored.Tokens[existing.Name], cfg.Defaults.LabelSelector, true)
		if err != nil {
			return err
		}
		projects = append(projects, project)
		finalStore.Tokens[project.Name] = token
		summaries = append(summaries, summary)
		seen[project.Name] = true
	}
	for {
		mustAdd := len(projects) == 0
		add := mustAdd
		if !mustAdd {
			add, err = w.prompt.Confirm("Add another project?", false)
			if err != nil {
				return err
			}
		}
		if !add {
			break
		}
		alias, err := w.newAlias(seen)
		if err != nil {
			return err
		}
		project, token, summary, err := w.configureProject(config.Project{Name: alias}, "", cfg.Defaults.LabelSelector, false)
		if err != nil {
			return err
		}
		projects = append(projects, project)
		finalStore.Tokens[alias] = token
		summaries = append(summaries, summary)
		seen[alias] = true
	}
	cfg.Projects = projects
	if err := cfg.Validate(); err != nil {
		return err
	}
	w.printSummary(cfg, summaries)
	confirmed, err := w.prompt.Confirm("Save this configuration?", false)
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("configuration cancelled; no files were changed")
	}
	configData, err := config.Marshal(cfg)
	if err != nil {
		return err
	}
	stagedStore := credentials.Store{Version: 1, Tokens: make(map[string]string, len(stored.Tokens)+len(finalStore.Tokens))}
	for name, token := range stored.Tokens {
		stagedStore.Tokens[name] = token
	}
	for name, token := range finalStore.Tokens {
		stagedStore.Tokens[name] = token
	}
	credentialData, err := credentials.Marshal(stagedStore)
	if err != nil {
		return err
	}
	if err := filetxn.WritePair(
		filetxn.File{Path: credentialPath, Data: credentialData, Mode: 0o600},
		filetxn.File{Path: w.configPath, Data: configData, Mode: 0o600},
	); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	if err := credentials.Save(credentialPath, finalStore); err != nil {
		fmt.Fprintf(w.out, "Warning: configuration was saved, but obsolete credentials could not be removed: %v\n", err)
	}
	fmt.Fprintf(successOut, "Configuration saved to %s\n", w.configPath)
	return nil
}

func (w *configureWizard) configureDefaults(d *config.Defaults) error {
	selector, err := w.prompt.Line("Global label selector (enter - to disable)", d.LabelSelector)
	if err != nil {
		return err
	}
	if selector == "-" {
		selector = ""
	}
	d.LabelSelector = selector
	if d.RetentionLabel, err = w.nonempty("Retention label", d.RetentionLabel); err != nil {
		return err
	}
	for {
		if d.KeepMax, err = w.positiveInt("Maximum snapshots to keep", d.KeepMax); err != nil {
			return err
		}
		if d.KeepLatest, err = w.positiveInt("Latest snapshots to keep", d.KeepLatest); err != nil {
			return err
		}
		if d.KeepTargetsRaw, d.KeepTargets, err = w.retentionTargets(d.KeepTargetsRaw); err != nil {
			return err
		}
		if d.KeepLatest+len(d.KeepTargets) <= d.KeepMax {
			break
		}
		fmt.Fprintln(w.out, "Latest snapshots plus age targets cannot exceed maximum snapshots.")
	}
	if d.SnapshotName, err = w.nonempty("Snapshot name format", d.SnapshotName); err != nil {
		return err
	}
	for {
		value, err := w.prompt.Line("Operation timeout", d.OperationTimeoutRaw)
		if err != nil {
			return err
		}
		duration, parseErr := time.ParseDuration(value)
		if parseErr == nil && duration > 0 {
			d.OperationTimeoutRaw = value
			d.OperationTimeout = duration
			break
		}
		fmt.Fprintln(w.out, "Operation timeout must be a positive Go duration such as 30m or 1h.")
	}
	if d.ProjectConcurrency, err = w.positiveInt("Concurrent projects", d.ProjectConcurrency); err != nil {
		return err
	}
	if d.ServerConcurrency, err = w.positiveInt("Concurrent servers per project", d.ServerConcurrency); err != nil {
		return err
	}
	return nil
}

func (w *configureWizard) retentionTargets(current []string) ([]string, []time.Duration, error) {
	for {
		value, err := w.prompt.Line("Snapshot age targets, youngest to oldest (comma-separated; - for none)", strings.Join(current, ", "))
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(value) == "-" || strings.TrimSpace(value) == "" {
			return []string{}, []time.Duration{}, nil
		}
		raw := strings.Split(value, ",")
		targets := make([]time.Duration, len(raw))
		valid := true
		for i := range raw {
			raw[i] = strings.TrimSpace(raw[i])
			target, parseErr := config.ParseRetentionDuration(raw[i])
			if parseErr != nil || target <= 0 || (i > 0 && target <= targets[i-1]) {
				valid = false
				break
			}
			targets[i] = target
		}
		if valid {
			return raw, targets, nil
		}
		fmt.Fprintln(w.out, "Age targets must be positive fixed durations ordered from youngest to oldest, such as 1d, 1w, 2w.")
	}
}

func (w *configureWizard) nonempty(label, current string) (string, error) {
	for {
		value, err := w.prompt.Line(label, current)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
		fmt.Fprintf(w.out, "%s cannot be empty.\n", label)
	}
}

func (w *configureWizard) positiveInt(label string, current int) (int, error) {
	for {
		value, err := w.prompt.Line(label, strconv.Itoa(current))
		if err != nil {
			return 0, err
		}
		parsed, parseErr := strconv.Atoi(value)
		if parseErr == nil && parsed >= 1 {
			return parsed, nil
		}
		fmt.Fprintf(w.out, "%s must be an integer of at least 1.\n", label)
	}
}

func (w *configureWizard) newAlias(seen map[string]bool) (string, error) {
	for {
		alias, err := w.prompt.Raw("Project alias: ")
		if err != nil {
			return "", err
		}
		if err := config.ValidateProjectName(alias); err != nil {
			fmt.Fprintln(w.out, err)
			continue
		}
		if seen[alias] {
			fmt.Fprintf(w.out, "Project %s is already configured.\n", alias)
			continue
		}
		return alias, nil
	}
}

func (w *configureWizard) configureProject(project config.Project, storedToken, selector string, existing bool) (config.Project, string, projectSummary, error) {
	fmt.Fprintf(w.out, "\nProject %s\n", project.Name)
	token, api, state, err := w.projectCredential(project.Name, storedToken, existing)
	if err != nil {
		return project, "", projectSummary{}, err
	}
	selected, total, err := w.pickServers(api, project, selector)
	if err != nil {
		return project, "", projectSummary{}, err
	}
	project.Include, project.Exclude = deriveOverrides(selected.servers, selected.matched, selected.selected)
	return project, token, projectSummary{name: project.Name, tokenState: state, selected: selected.count(), total: total}, nil
}

func (w *configureWizard) projectCredential(alias, storedToken string, existing bool) (string, projectAPI, string, error) {
	if existing && storedToken != "" {
		api := w.factory(storedToken)
		checkCtx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
		err := api.Validate(checkCtx)
		cancel()
		if err == nil {
			replace, err := w.prompt.Confirm("Replace the stored API token?", false)
			if err != nil {
				return "", nil, "", err
			}
			if !replace {
				return storedToken, api, "kept", nil
			}
		} else {
			fmt.Fprintf(w.out, "The stored API token is invalid: %v\n", err)
		}
	}
	for {
		token, err := w.prompt.Secret(fmt.Sprintf("Hetzner API token for %s: ", alias))
		if err != nil {
			return "", nil, "", err
		}
		if token == "" {
			fmt.Fprintln(w.out, "Token cannot be empty.")
			continue
		}
		api := w.factory(token)
		checkCtx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
		err = api.Validate(checkCtx)
		cancel()
		if err != nil {
			fmt.Fprintf(w.out, "Token validation failed: %v\n", err)
			continue
		}
		state := "added"
		if existing {
			state = "replaced"
		}
		return token, api, state, nil
	}
}

type pickerState struct {
	servers           []*hcloud.Server
	matched, selected map[int64]bool
}

func (p pickerState) count() int {
	count := 0
	for _, yes := range p.selected {
		if yes {
			count++
		}
	}
	return count
}

func (w *configureWizard) pickServers(api projectAPI, project config.Project, selector string) (pickerState, int, error) {
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	servers, err := api.AllServers(ctx)
	if err != nil {
		return pickerState{}, 0, fmt.Errorf("list servers for project %s: %w", project.Name, err)
	}
	matchedServers, err := api.SelectorServers(ctx, selector)
	if err != nil {
		return pickerState{}, 0, fmt.Errorf("apply selector for project %s: %w", project.Name, err)
	}
	state := pickerState{servers: servers, matched: map[int64]bool{}, selected: map[int64]bool{}}
	knownIDs := make(map[int64]bool, len(servers))
	for _, server := range servers {
		knownIDs[server.ID] = true
	}
	for _, server := range matchedServers {
		if !knownIDs[server.ID] {
			continue
		}
		state.matched[server.ID] = true
		state.selected[server.ID] = true
	}
	byID := map[string]*hcloud.Server{}
	byName := map[string]*hcloud.Server{}
	for _, server := range servers {
		byID[strconv.FormatInt(server.ID, 10)] = server
		byName[server.Name] = server
	}
	applyRefs := func(refs []string, value bool) {
		for _, ref := range refs {
			parts := strings.SplitN(ref, ":", 2)
			var server *hcloud.Server
			if len(parts) == 2 && parts[0] == "id" {
				server = byID[parts[1]]
			} else if len(parts) == 2 && parts[0] == "name" {
				server = byName[parts[1]]
			}
			if server == nil {
				fmt.Fprintf(w.out, "Discarding stale server reference %s from project %s.\n", ref, project.Name)
				continue
			}
			state.selected[server.ID] = value
		}
	}
	applyRefs(project.Include, true)
	applyRefs(project.Exclude, false)
	if len(servers) == 0 {
		fmt.Fprintln(w.out, "No servers exist in this project.")
		return state, 0, nil
	}
	if w.picker == nil {
		return pickerState{}, 0, fmt.Errorf("server picker is not configured")
	}
	selected, err := w.picker(w.ctx, w.out, project.Name, state)
	if err != nil {
		return pickerState{}, 0, err
	}
	state.selected = selected
	return state, len(servers), nil
}

func deriveOverrides(servers []*hcloud.Server, matched, selected map[int64]bool) ([]string, []string) {
	var includeIDs, excludeIDs []int64
	for _, server := range servers {
		if selected[server.ID] && !matched[server.ID] {
			includeIDs = append(includeIDs, server.ID)
		}
		if !selected[server.ID] && matched[server.ID] {
			excludeIDs = append(excludeIDs, server.ID)
		}
	}
	sort.Slice(includeIDs, func(i, j int) bool { return includeIDs[i] < includeIDs[j] })
	sort.Slice(excludeIDs, func(i, j int) bool { return excludeIDs[i] < excludeIDs[j] })
	include := make([]string, len(includeIDs))
	for i, id := range includeIDs {
		include[i] = "id:" + strconv.FormatInt(id, 10)
	}
	exclude := make([]string, len(excludeIDs))
	for i, id := range excludeIDs {
		exclude[i] = "id:" + strconv.FormatInt(id, 10)
	}
	return include, exclude
}

func (w *configureWizard) printSummary(cfg config.Config, projects []projectSummary) {
	selector := cfg.Defaults.LabelSelector
	if selector == "" {
		selector = "<disabled>"
	}
	fmt.Fprintln(w.out, "\nConfiguration summary")
	targets := strings.Join(cfg.Defaults.KeepTargetsRaw, ", ")
	if targets == "" {
		targets = "<none>"
	}
	fmt.Fprintf(w.out, "  Selector: %s\n  Retention label: %s\n  Keep: maximum %d, latest %d, age targets %s\n  Snapshot name: %s\n  Timeout: %s\n  Concurrency: %d projects, %d servers per project\n", selector, cfg.Defaults.RetentionLabel, cfg.Defaults.KeepMax, cfg.Defaults.KeepLatest, targets, cfg.Defaults.SnapshotName, cfg.Defaults.OperationTimeoutRaw, cfg.Defaults.ProjectConcurrency, cfg.Defaults.ServerConcurrency)
	for _, project := range projects {
		fmt.Fprintf(w.out, "  Project %s: %d/%d servers selected; token %s\n", project.name, project.selected, project.total, project.tokenState)
	}
}
