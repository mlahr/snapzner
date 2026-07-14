package cmd

import (
	"context"
	"fmt"
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
