package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/mlahr/snapzner/internal/config"
	"github.com/mlahr/snapzner/internal/credentials"
	"github.com/mlahr/snapzner/internal/snapzner"
)

type filteredBackupRun struct {
	project config.Project
	service *snapzner.Service
	servers []*hcloud.Server
}

// parseDiscoveredServerIDs recognizes the cross-project discovery form. It is
// deliberately limited to unqualified IDs without --project so existing name,
// qualified-target, and explicitly scoped behavior remains unchanged.
func parseDiscoveredServerIDs(values, projectFlags []string) ([]int64, bool, error) {
	if len(values) == 0 || len(projectFlags) > 0 {
		return nil, false, nil
	}
	ids := make([]int64, 0, len(values))
	seen := make(map[int64]bool, len(values))
	for _, value := range values {
		raw := value
		explicitID := strings.HasPrefix(raw, "id:")
		if explicitID {
			raw = strings.TrimPrefix(raw, "id:")
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			if explicitID {
				return nil, false, fmt.Errorf("discovered server ID %q must be a positive integer", value)
			}
			return nil, false, nil
		}
		if id <= 0 {
			return nil, false, fmt.Errorf("discovered server ID %q must be a positive integer", value)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true, nil
}

func parseBackupTargets(values, projectFlags []string) (map[string][]string, []string, error) {
	if len(values) == 0 {
		return nil, nil, nil
	}

	projects := uniqueStrings(projectFlags)
	projectSet := make(map[string]bool, len(projects))
	for _, project := range projects {
		if project == "" {
			return nil, nil, fmt.Errorf("--project cannot be empty when --server is used")
		}
		projectSet[project] = true
	}

	targets := map[string][]string{}
	var derivedProjects, unqualified []string
	for _, value := range values {
		if value == "" {
			return nil, nil, fmt.Errorf("--server cannot be empty")
		}
		project, server, qualified := strings.Cut(value, "/")
		if !qualified {
			unqualified = append(unqualified, value)
			continue
		}
		if project == "" || server == "" || strings.Contains(server, "/") {
			return nil, nil, fmt.Errorf("invalid qualified server %q; expected PROJECT/SERVER", value)
		}
		if _, exists := targets[project]; !exists {
			derivedProjects = append(derivedProjects, project)
		}
		targets[project] = appendUnique(targets[project], server)
	}

	if len(unqualified) > 0 {
		if len(projects) != 1 {
			return nil, nil, fmt.Errorf("unqualified --server values require exactly one --project")
		}
		for _, server := range unqualified {
			targets[projects[0]] = appendUnique(targets[projects[0]], server)
		}
		if len(derivedProjects) == 0 {
			derivedProjects = append(derivedProjects, projects[0])
		}
	}

	if len(projects) == 0 {
		return targets, derivedProjects, nil
	}
	for project := range targets {
		if !projectSet[project] {
			return nil, nil, fmt.Errorf("qualified server project %q is not selected by --project", project)
		}
	}
	for _, project := range projects {
		if len(targets[project]) == 0 {
			return nil, nil, fmt.Errorf("project %q has no --server target", project)
		}
	}
	return targets, projects, nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (a *app) runFilteredBackup(ctx context.Context, projectNames []string, targets map[string][]string, report func(snapzner.Progress)) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	store, err := credentials.Load(credentials.PathForConfig(a.configPath))
	if err != nil {
		return err
	}
	projects, err := selectProjects(cfg, projectNames)
	if err != nil {
		return err
	}

	runs := make([]filteredBackupRun, 0, len(projects))
	var preflightEvents []snapzner.Event
	for _, project := range projects {
		service, err := a.serviceForProject(cfg, store, project)
		if err != nil {
			preflightEvents = append(preflightEvents, snapzner.Event{
				Project: project.Name, Operation: "project", Message: "credential unavailable", Error: err.Error(),
			})
			continue
		}
		service.OnProgress = report
		runs = append(runs, filteredBackupRun{project: project, service: service})
	}
	if len(preflightEvents) > 0 {
		return a.finishEvents(preflightEvents, true)
	}

	type selectionResult struct {
		index   int
		servers []*hcloud.Server
		err     error
	}
	selectionResults := make(chan selectionResult, len(runs))
	sem := make(chan struct{}, cfg.Defaults.ProjectConcurrency)
	var wg sync.WaitGroup
	for index := range runs {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			run := runs[index]
			servers, err := run.service.SelectBackupServers(ctx, run.project, targets[run.project.Name])
			selectionResults <- selectionResult{index: index, servers: servers, err: err}
		}()
	}
	wg.Wait()
	close(selectionResults)
	for result := range selectionResults {
		if result.err != nil {
			preflightEvents = append(preflightEvents, snapzner.Event{
				Project: runs[result.index].project.Name, Operation: "backup", Message: "server selection failed", Error: result.err.Error(),
			})
			continue
		}
		runs[result.index].servers = result.servers
	}
	if len(preflightEvents) > 0 {
		return a.finishEvents(preflightEvents, true)
	}

	results := make(chan []snapzner.Event, len(runs))
	for index := range runs {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- runs[index].service.BackupServers(ctx, runs[index].servers)
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
	return a.finishEvents(events, failed)
}

func (a *app) runDiscoveredIDBackup(ctx context.Context, ids []int64, report func(snapzner.Progress)) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	store, err := credentials.Load(credentials.PathForConfig(a.configPath))
	if err != nil {
		return err
	}
	projects, err := selectProjects(cfg, nil)
	if err != nil {
		return err
	}

	runs := make([]filteredBackupRun, 0, len(projects))
	var preflightEvents []snapzner.Event
	for _, project := range projects {
		service, err := a.serviceForProject(cfg, store, project)
		if err != nil {
			preflightEvents = append(preflightEvents, snapzner.Event{
				Project: project.Name, Operation: "project", Message: "credential unavailable", Error: err.Error(),
			})
			continue
		}
		service.OnProgress = report
		runs = append(runs, filteredBackupRun{project: project, service: service})
	}
	if len(preflightEvents) > 0 {
		return a.finishEvents(preflightEvents, true)
	}

	type selectionResult struct {
		index   int
		servers []*hcloud.Server
		err     error
	}
	selectionResults := make(chan selectionResult, len(runs))
	sem := make(chan struct{}, cfg.Defaults.ProjectConcurrency)
	var wg sync.WaitGroup
	for index := range runs {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			run := runs[index]
			servers, err := run.service.SelectBackupServers(ctx, run.project, nil)
			selectionResults <- selectionResult{index: index, servers: servers, err: err}
		}()
	}
	wg.Wait()
	close(selectionResults)
	for result := range selectionResults {
		if result.err != nil {
			preflightEvents = append(preflightEvents, snapzner.Event{
				Project: runs[result.index].project.Name, Operation: "discover", Message: "managed server discovery failed", Error: result.err.Error(),
			})
			continue
		}
		runs[result.index].servers = result.servers
	}
	if len(preflightEvents) > 0 {
		return a.finishEvents(preflightEvents, true)
	}

	requested := make(map[int64]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	type match struct {
		runIndex int
		server   *hcloud.Server
	}
	matches := make(map[int64][]match, len(ids))
	for index := range runs {
		selected := runs[index].servers
		runs[index].servers = nil
		for _, server := range selected {
			if requested[server.ID] {
				matches[server.ID] = append(matches[server.ID], match{runIndex: index, server: server})
			}
		}
	}
	var discoveryEvents []snapzner.Event
	failed := false
	for _, id := range ids {
		switch len(matches[id]) {
		case 0:
			discoveryEvents = append(discoveryEvents, snapzner.Event{
				Operation: "discover", ResourceID: id,
				Message: "server is not selected by any configured project; skipped",
			})
		case 1:
			matched := matches[id][0]
			runs[matched.runIndex].servers = append(runs[matched.runIndex].servers, matched.server)
		default:
			failed = true
			var names []string
			for _, matched := range matches[id] {
				names = append(names, runs[matched.runIndex].project.Name)
			}
			discoveryEvents = append(discoveryEvents, snapzner.Event{
				Operation: "discover", ResourceID: id, Message: "server ID matched multiple configured projects",
				Error: "matched projects: " + strings.Join(names, ", "),
			})
		}
	}
	if failed {
		return a.finishEvents(discoveryEvents, true)
	}

	results := make(chan []snapzner.Event, len(runs))
	for index := range runs {
		if len(runs[index].servers) == 0 {
			continue
		}
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- runs[index].service.BackupServers(ctx, runs[index].servers)
		}()
	}
	wg.Wait()
	close(results)

	events := discoveryEvents
	for batch := range results {
		for _, event := range batch {
			if event.Error != "" {
				failed = true
			}
			events = append(events, event)
		}
	}
	return a.finishEvents(events, failed)
}
