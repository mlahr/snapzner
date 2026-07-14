package snapzner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/mlahr/snapzner/internal/config"
)

type Event struct {
	Project    string `json:"project,omitempty"`
	Operation  string `json:"operation"`
	ResourceID int64  `json:"resource_id,omitempty"`
	Message    string `json:"message"`
	Error      string `json:"error,omitempty"`
}

// Progress describes transient backup status. It is not a final result event.
type Progress struct {
	Project    string
	Message    string
	ServerID   int64
	ServerName string
	Completed  int
	Total      int
}

type Service struct {
	Project           string
	Cloud             *Cloud
	Policy            config.Policy
	Timeout           time.Duration
	ServerConcurrency int
	OnProgress        func(Progress)
	progressMu        sync.Mutex
}

func (s *Service) event(operation string, id int64, message string, err error) Event {
	e := Event{Project: s.Project, Operation: operation, ResourceID: id, Message: message}
	if err != nil {
		e.Error = err.Error()
	}
	return e
}

func (s *Service) progress(message string, server *hcloud.Server, completed, total int) {
	if s.OnProgress == nil {
		return
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	progress := Progress{Project: s.Project, Message: message, Completed: completed, Total: total}
	if server != nil {
		progress.ServerID = server.ID
		progress.ServerName = server.Name
	}
	s.OnProgress(progress)
}

func (s *Service) Backup(ctx context.Context, project config.Project) []Event {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	s.progress("selecting servers", nil, 0, 0)
	servers, err := s.selectBackupServers(ctx, project, nil)
	if err != nil {
		return []Event{s.event("backup", 0, "server selection failed", err)}
	}
	return s.backupServers(ctx, servers)
}

// SelectBackupServers resolves and validates an exact per-run server filter
// against the servers selected by the persisted project configuration. It
// performs no mutations and is suitable for an all-project preflight.
func (s *Service) SelectBackupServers(ctx context.Context, project config.Project, requested []string) ([]*hcloud.Server, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	s.progress("selecting servers", nil, 0, 0)
	return s.selectBackupServers(ctx, project, requested)
}

func (s *Service) selectBackupServers(ctx context.Context, project config.Project, requested []string) ([]*hcloud.Server, error) {
	selected, err := s.Cloud.SelectedServers(ctx, s.Policy.LabelSelector, project.Include, project.Exclude)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		return selected, nil
	}

	eligible := make(map[int64]*hcloud.Server, len(selected))
	for _, server := range selected {
		eligible[server.ID] = server
	}
	targets := make(map[int64]*hcloud.Server, len(requested))
	for _, value := range requested {
		server, err := s.Cloud.ResolveServerValue(ctx, value)
		if err != nil {
			return nil, fmt.Errorf("requested server %q: %w", value, err)
		}
		configured, ok := eligible[server.ID]
		if !ok {
			return nil, fmt.Errorf("requested server %q (%s, id %d) is not selected by project configuration", value, server.Name, server.ID)
		}
		targets[server.ID] = configured
	}
	servers := make([]*hcloud.Server, 0, len(targets))
	for _, server := range targets {
		servers = append(servers, server)
	}
	sortServers(servers)
	return servers, nil
}

// BackupServers creates snapshots for a prevalidated exact server list.
func (s *Service) BackupServers(ctx context.Context, servers []*hcloud.Server) []Event {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	return s.backupServers(ctx, servers)
}

func (s *Service) backupServers(ctx context.Context, servers []*hcloud.Server) []Event {
	if len(servers) == 0 {
		s.progress("no servers selected", nil, 0, 0)
		return []Event{s.event("backup", 0, "no servers selected", nil)}
	}
	s.progress(fmt.Sprintf("selected %d %s", len(servers), plural(len(servers), "server", "servers")), nil, 0, len(servers))

	jobs := make(chan *hcloud.Server)
	events := make(chan Event, len(servers)*2)
	succeeded := make(chan int64, len(servers))
	workers := s.ServerConcurrency
	if workers > len(servers) {
		workers = len(servers)
	}
	completed := 0
	var completionMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for server := range jobs {
				s.progress("creating snapshot", server, 0, len(servers))
				e, ok := s.createSnapshot(ctx, server)
				events <- e
				if ok {
					succeeded <- server.ID
				}
				completionMu.Lock()
				completed++
				current := completed
				message := "snapshot available"
				if !ok {
					message = "snapshot failed"
				}
				s.progress(message, server, current, len(servers))
				completionMu.Unlock()
			}
		}()
	}
	go func() {
		for _, server := range servers {
			jobs <- server
		}
		close(jobs)
		wg.Wait()
		close(events)
		close(succeeded)
	}()
	var result []Event
	for e := range events {
		result = append(result, e)
	}
	successIDs := map[int64]bool{}
	for id := range succeeded {
		successIDs[id] = true
	}
	if len(successIDs) > 0 {
		s.progress(fmt.Sprintf("enforcing retention for %d %s", len(successIDs), plural(len(successIDs), "server", "servers")), nil, len(servers), len(servers))
		pruneEvents := s.prune(ctx, true, successIDs, false)
		result = append(result, pruneEvents...)
	}
	s.progress(fmt.Sprintf("backup finished: %d/%d snapshots created", len(successIDs), len(servers)), nil, len(servers), len(servers))
	return result
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func (s *Service) createSnapshot(ctx context.Context, server *hcloud.Server) (Event, bool) {
	keep := s.Policy.KeepLast
	if raw, ok := server.Labels[s.Policy.RetentionLabel]; ok {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return s.event("backup", server.ID, "invalid server retention label", fmt.Errorf("%s=%q must be an integer of at least 1", s.Policy.RetentionLabel, raw)), false
		}
		keep = value
	}
	labels := make(map[string]string, len(server.Labels)+10)
	for k, v := range server.Labels {
		if !strings.HasPrefix(k, metadataPrefix) {
			labels[k] = v
		}
	}
	labels[metadataPrefix+"managed"] = "v1"
	labels[metadataPrefix+"source-id"] = strconv.FormatInt(server.ID, 10)
	labels[metadataPrefix+"source-name"] = server.Name
	labels[metadataPrefix+"server-type"] = server.ServerType.Name
	labels[metadataPrefix+"location"] = server.Location.Name
	labels[metadataPrefix+"ipv4"] = strconv.FormatBool(!server.PublicNet.IPv4.IsUnspecified())
	labels[metadataPrefix+"ipv6"] = strconv.FormatBool(!server.PublicNet.IPv6.IsUnspecified())
	labels[metadataPrefix+"keep-last"] = strconv.Itoa(keep)
	firewalls, err := s.Cloud.DirectFirewallIDs(ctx, server.ID)
	if err != nil {
		return s.event("backup", server.ID, "could not inspect directly attached firewalls", err), false
	}
	for _, id := range firewalls {
		labels[metadataPrefix+"firewall-"+strconv.FormatInt(id, 10)] = "true"
	}
	description := RenderName(s.Policy.SnapshotName, s.Project, server, time.Now().UTC())
	created, _, err := s.Cloud.Client.Server.CreateImage(ctx, server, &hcloud.ServerCreateImageOpts{
		Type: hcloud.ImageTypeSnapshot, Description: &description, Labels: labels,
	})
	if err != nil {
		return s.event("backup", server.ID, "snapshot creation failed", err), false
	}
	if err := s.Cloud.Client.Action.WaitFor(ctx, created.Action); err != nil {
		return s.event("backup", created.Image.ID, "snapshot action failed", err), false
	}
	return s.event("backup", created.Image.ID, fmt.Sprintf("snapshot available for server %s (%d)", server.Name, server.ID), nil), true
}

func RenderName(pattern, project string, server *hcloud.Server, now time.Time) string {
	r := strings.NewReplacer(
		"%project%", project,
		"%id%", strconv.FormatInt(server.ID, 10),
		"%name%", server.Name,
		"%timestamp%", strconv.FormatInt(now.Unix(), 10),
		"%date%", now.Format("2006-01-02"),
		"%time%", now.Format("15:04:05"),
	)
	return r.Replace(pattern)
}

func (s *Service) Prune(ctx context.Context, apply, force bool) []Event {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	return s.prune(ctx, apply, nil, force)
}

func (s *Service) prune(ctx context.Context, apply bool, restrict map[int64]bool, force bool) []Event {
	images, err := s.Cloud.ManagedSnapshots(ctx)
	if err != nil {
		return []Event{s.event("prune", 0, "could not list managed snapshots", err)}
	}
	groups := map[int64][]*hcloud.Image{}
	for _, image := range images {
		id, err := strconv.ParseInt(image.Labels[metadataPrefix+"source-id"], 10, 64)
		if err != nil {
			continue
		}
		if restrict != nil && !restrict[id] {
			continue
		}
		groups[id] = append(groups[id], image)
	}
	var events []Event
	now := time.Now().UTC()
	for sourceID, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].Created.After(group[j].Created) })
		for _, image := range PruneCandidates(group, s.Policy, now) {
			if image.Protection.Delete && !force {
				events = append(events, s.event("prune", image.ID, "snapshot is deletion-protected; retained", nil))
				continue
			}
			if !apply {
				events = append(events, s.event("prune", image.ID, fmt.Sprintf("would delete snapshot for source %d", sourceID), nil))
				continue
			}
			if image.Protection.Delete {
				if err := s.setImageDeleteProtection(ctx, image, false); err != nil {
					events = append(events, s.event("prune", image.ID, "could not disable snapshot deletion protection", err))
					continue
				}
			}
			_, err := s.Cloud.Client.Image.Delete(ctx, image)
			if err != nil {
				events = append(events, s.event("prune", image.ID, "snapshot deletion failed", err))
			} else {
				events = append(events, s.event("prune", image.ID, fmt.Sprintf("deleted snapshot for source %d", sourceID), nil))
			}
		}
	}
	if len(events) == 0 {
		events = append(events, s.event("prune", 0, "no snapshots exceed retention", nil))
	}
	return events
}

// PruneCandidates returns deletable snapshots from an input ordered newest
// first. KeepMin is an absolute floor; MaxAge may override KeepLast but never
// KeepMin. The newest snapshot carries any per-server KeepLast override.
func PruneCandidates(images []*hcloud.Image, policy config.Policy, now time.Time) []*hcloud.Image {
	keepMin := policy.KeepMin
	if keepMin < 1 {
		keepMin = 1
	}
	keepLast := policy.KeepLast
	if len(images) > 0 {
		if value, err := strconv.Atoi(images[0].Labels[metadataPrefix+"keep-last"]); err == nil && value >= 1 {
			keepLast = value
		}
	}
	if keepLast < keepMin {
		keepLast = keepMin
	}

	var candidates []*hcloud.Image
	for index, image := range images {
		if index < keepMin {
			continue
		}
		age := now.Sub(image.Created)
		if policy.MaxAge > 0 && age >= policy.MaxAge {
			candidates = append(candidates, image)
			continue
		}
		if index >= keepLast && (policy.MinAge <= 0 || age >= policy.MinAge) {
			candidates = append(candidates, image)
		}
	}
	return candidates
}

func (s *Service) ListSnapshots(ctx context.Context, all bool) ([]*hcloud.Image, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	if all {
		return s.Cloud.AllSnapshots(ctx)
	}
	return s.Cloud.ManagedSnapshots(ctx)
}

func (s *Service) DeleteSnapshots(ctx context.Context, ids []int64, force bool) []Event {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	var events []Event
	for _, id := range ids {
		image, _, err := s.Cloud.Client.Image.GetByID(ctx, id)
		if err != nil {
			events = append(events, s.event("delete", id, "could not read snapshot", err))
			continue
		}
		if image == nil {
			events = append(events, s.event("delete", id, "snapshot not found", fmt.Errorf("not found")))
			continue
		}
		if image.Type != hcloud.ImageTypeSnapshot {
			events = append(events, s.event("delete", id, "refusing to delete a non-snapshot image", fmt.Errorf("image type is %s", image.Type)))
			continue
		}
		if image.Labels[metadataPrefix+"managed"] != "v1" && !force {
			events = append(events, s.event("delete", id, "refusing unmanaged snapshot", fmt.Errorf("pass --force to override")))
			continue
		}
		if image.Protection.Delete && !force {
			events = append(events, s.event("delete", id, "snapshot is deletion-protected", fmt.Errorf("delete protection is enabled")))
			continue
		}
		if image.Protection.Delete {
			if err := s.setImageDeleteProtection(ctx, image, false); err != nil {
				events = append(events, s.event("delete", id, "could not disable snapshot deletion protection", err))
				continue
			}
		}
		_, err = s.Cloud.Client.Image.Delete(ctx, image)
		if err != nil {
			events = append(events, s.event("delete", id, "snapshot deletion failed", err))
		} else {
			events = append(events, s.event("delete", id, "snapshot deleted", nil))
		}
	}
	return events
}

func (s *Service) setImageDeleteProtection(ctx context.Context, image *hcloud.Image, enabled bool) error {
	action, _, err := s.Cloud.Client.Image.ChangeProtection(ctx, image, hcloud.ImageChangeProtectionOpts{Delete: &enabled})
	if err != nil {
		return err
	}
	return s.Cloud.Client.Action.WaitFor(ctx, action)
}

func (s *Service) ResolveSnapshot(ctx context.Context, value, source string) (*hcloud.Image, error) {
	if value != "latest" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("snapshot must be an integer ID or latest")
		}
		image, _, err := s.Cloud.Client.Image.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if image == nil || image.Type != hcloud.ImageTypeSnapshot {
			return nil, fmt.Errorf("snapshot %d was not found", id)
		}
		if image.Status != hcloud.ImageStatusAvailable {
			return nil, fmt.Errorf("snapshot %d is not available", id)
		}
		return image, nil
	}
	if source == "" {
		return nil, fmt.Errorf("--source is required with --snapshot latest")
	}
	server, err := s.Cloud.ResolveServerValue(ctx, source)
	if err != nil {
		return nil, err
	}
	images, err := s.Cloud.ManagedSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	var matches []*hcloud.Image
	for _, image := range images {
		if image.Labels[metadataPrefix+"source-id"] == strconv.FormatInt(server.ID, 10) && image.Status == hcloud.ImageStatusAvailable {
			matches = append(matches, image)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no managed snapshot found for server %s", server.Name)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Created.After(matches[j].Created) })
	return matches[0], nil
}

type CloneOptions struct {
	Snapshot, Source, Name, ServerType, Location string
	EnableIPv4, EnableIPv6                       *bool
}

func (s *Service) Clone(ctx context.Context, opts CloneOptions) []Event {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	image, err := s.ResolveSnapshot(ctx, opts.Snapshot, opts.Source)
	if err != nil {
		return []Event{s.event("clone", 0, "snapshot resolution failed", err)}
	}
	serverType := firstNonempty(opts.ServerType, image.Labels[metadataPrefix+"server-type"])
	location := firstNonempty(opts.Location, image.Labels[metadataPrefix+"location"])
	if serverType == "" || location == "" {
		return []Event{s.event("clone", image.ID, "snapshot lacks replay metadata", fmt.Errorf("--server-type and --location are required"))}
	}
	name := opts.Name
	if name == "" {
		name = firstNonempty(image.Labels[metadataPrefix+"source-name"], "snapshot") + "-replay-" + time.Now().UTC().Format("20060102-150405")
	}
	ipv4 := parseBoolDefault(image.Labels[metadataPrefix+"ipv4"], true)
	if opts.EnableIPv4 != nil {
		ipv4 = *opts.EnableIPv4
	}
	ipv6 := parseBoolDefault(image.Labels[metadataPrefix+"ipv6"], true)
	if opts.EnableIPv6 != nil {
		ipv6 = *opts.EnableIPv6
	}
	labels := map[string]string{}
	for k, v := range image.Labels {
		if strings.HasPrefix(k, metadataPrefix) || k == s.Policy.RetentionLabel {
			continue
		}
		labels[k] = v
	}
	if key := selectorKey(s.Policy.LabelSelector); key != "" {
		delete(labels, key)
	}
	labels[metadataPrefix+"replay"] = "true"
	labels[metadataPrefix+"replay-source"] = strconv.FormatInt(image.ID, 10)
	var firewalls []*hcloud.ServerCreateFirewall
	for key := range image.Labels {
		if !strings.HasPrefix(key, metadataPrefix+"firewall-") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(key, metadataPrefix+"firewall-"), 10, 64)
		if err != nil {
			continue
		}
		fw, _, err := s.Cloud.Client.Firewall.GetByID(ctx, id)
		if err != nil || fw == nil {
			return []Event{s.event("clone", image.ID, "recorded firewall is unavailable", fmt.Errorf("firewall %d", id))}
		}
		firewalls = append(firewalls, &hcloud.ServerCreateFirewall{Firewall: *fw})
	}
	start := true
	created, _, err := s.Cloud.Client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name: name, ServerType: &hcloud.ServerType{Name: serverType}, Image: image,
		Location: &hcloud.Location{Name: location}, Labels: labels, Firewalls: firewalls,
		PublicNet: &hcloud.ServerCreatePublicNet{EnableIPv4: ipv4, EnableIPv6: ipv6}, StartAfterCreate: &start,
	})
	if err != nil {
		return []Event{s.event("clone", image.ID, "server creation failed", err)}
	}
	actions := append([]*hcloud.Action{created.Action}, created.NextActions...)
	if err := s.Cloud.Client.Action.WaitFor(ctx, actions...); err != nil {
		return []Event{s.event("clone", created.Server.ID, "server creation action failed", err)}
	}
	return []Event{s.event("clone", created.Server.ID, fmt.Sprintf("created replay server %s from snapshot %d", name, image.ID), nil)}
}

func (s *Service) Rebuild(ctx context.Context, snapshot, source, target string, force bool) []Event {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	image, err := s.ResolveSnapshot(ctx, snapshot, source)
	if err != nil {
		return []Event{s.event("rebuild", 0, "snapshot resolution failed", err)}
	}
	server, err := s.Cloud.ResolveServerValue(ctx, target)
	if err != nil {
		return []Event{s.event("rebuild", image.ID, "target resolution failed", err)}
	}
	if server.Protection.Rebuild && !force {
		return []Event{s.event("rebuild", server.ID, "target server is rebuild-protected", fmt.Errorf("pass --force to temporarily disable rebuild protection"))}
	}
	restoreProtection := func() error { return nil }
	if server.Protection.Rebuild {
		disabled := false
		action, _, err := s.Cloud.Client.Server.ChangeProtection(ctx, server, hcloud.ServerChangeProtectionOpts{Rebuild: &disabled})
		if err != nil {
			return []Event{s.event("rebuild", server.ID, "could not disable rebuild protection", err)}
		}
		if err := s.Cloud.Client.Action.WaitFor(ctx, action); err != nil {
			return []Event{s.event("rebuild", server.ID, "could not disable rebuild protection", err)}
		}
		restoreProtection = func() error {
			enabled := true
			action, _, err := s.Cloud.Client.Server.ChangeProtection(ctx, server, hcloud.ServerChangeProtectionOpts{Rebuild: &enabled})
			if err != nil {
				return err
			}
			return s.Cloud.Client.Action.WaitFor(ctx, action)
		}
	}
	result, _, err := s.Cloud.Client.Server.RebuildWithResult(ctx, server, hcloud.ServerRebuildOpts{Image: image})
	if err != nil {
		return []Event{s.event("rebuild", server.ID, "rebuild could not be started", errors.Join(err, restoreProtection()))}
	}
	if err := s.Cloud.Client.Action.WaitFor(ctx, result.Action); err != nil {
		return []Event{s.event("rebuild", server.ID, "rebuild action failed", errors.Join(err, restoreProtection()))}
	}
	if err := restoreProtection(); err != nil {
		return []Event{s.event("rebuild", server.ID, "server rebuilt but rebuild protection could not be restored", err)}
	}
	return []Event{s.event("rebuild", server.ID, fmt.Sprintf("rebuilt server %s from snapshot %d", server.Name, image.ID), nil)}
}

func firstNonempty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func parseBoolDefault(value string, fallback bool) bool {
	v, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return v
}
func selectorKey(selector string) string {
	for i, r := range selector {
		if r == '=' || r == '!' || r == ' ' || r == ',' {
			return selector[:i]
		}
	}
	return selector
}
