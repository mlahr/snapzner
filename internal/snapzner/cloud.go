package snapzner

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const managedSelector = "snapzner.mlahr.dev/managed=v1"
const metadataPrefix = "snapzner.mlahr.dev/"

type Cloud struct{ Client *hcloud.Client }

func NewCloud(token, version string) *Cloud {
	opts := []hcloud.ClientOption{
		hcloud.WithToken(token),
		hcloud.WithApplication("snapzner", version),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ExponentialBackoff(1.5, time.Second)}),
	}
	if endpoint := os.Getenv("SNAPZNER_API_ENDPOINT"); endpoint != "" {
		opts = append(opts, hcloud.WithEndpoint(endpoint))
	}
	return &Cloud{Client: hcloud.NewClient(opts...)}
}

func (c *Cloud) Validate(ctx context.Context) error {
	_, _, err := c.Client.Server.List(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{PerPage: 1}})
	return err
}

func (c *Cloud) AllServers(ctx context.Context) ([]*hcloud.Server, error) {
	servers, err := c.Client.Server.All(ctx)
	if err != nil {
		return nil, err
	}
	sortServers(servers)
	return servers, nil
}

func (c *Cloud) SelectorServers(ctx context.Context, selector string) ([]*hcloud.Server, error) {
	if selector == "" {
		return nil, nil
	}
	servers, err := c.Client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: selector}})
	if err != nil {
		return nil, err
	}
	sortServers(servers)
	return servers, nil
}

func (c *Cloud) SelectedServers(ctx context.Context, selector string, include, exclude []string) ([]*hcloud.Server, error) {
	selected := map[int64]*hcloud.Server{}
	if selector != "" {
		servers, err := c.Client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: selector}})
		if err != nil {
			return nil, fmt.Errorf("select servers with %q: %w", selector, err)
		}
		for _, server := range servers {
			selected[server.ID] = server
		}
	}
	for _, ref := range include {
		server, err := c.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		selected[server.ID] = server
	}
	for _, ref := range exclude {
		server, err := c.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		delete(selected, server.ID)
	}
	servers := make([]*hcloud.Server, 0, len(selected))
	for _, server := range selected {
		servers = append(servers, server)
	}
	sortServers(servers)
	return servers, nil
}

func (c *Cloud) resolveServer(ctx context.Context, ref string) (*hcloud.Server, error) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, fmt.Errorf("invalid server reference %q", ref)
	}
	var server *hcloud.Server
	var err error
	switch parts[0] {
	case "id":
		id, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid server ID in %q", ref)
		}
		server, _, err = c.Client.Server.GetByID(ctx, id)
	case "name":
		server, _, err = c.Client.Server.GetByName(ctx, parts[1])
	default:
		return nil, fmt.Errorf("invalid server reference %q", ref)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve server %q: %w", ref, err)
	}
	if server == nil {
		return nil, fmt.Errorf("server %q was not found", ref)
	}
	return server, nil
}

func (c *Cloud) ResolveServerValue(ctx context.Context, value string) (*hcloud.Server, error) {
	if strings.HasPrefix(value, "id:") || strings.HasPrefix(value, "name:") {
		return c.resolveServer(ctx, value)
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return c.resolveServer(ctx, "id:"+value)
	}
	return c.resolveServer(ctx, "name:"+value)
}

func (c *Cloud) DirectFirewallIDs(ctx context.Context, serverID int64) ([]int64, error) {
	firewalls, err := c.Client.Firewall.All(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, fw := range firewalls {
		for _, target := range fw.AppliedTo {
			if target.Type == hcloud.FirewallResourceTypeServer && target.Server != nil && target.Server.ID == serverID {
				ids = append(ids, fw.ID)
				break
			}
		}
	}
	return ids, nil
}

func (c *Cloud) ManagedSnapshots(ctx context.Context) ([]*hcloud.Image, error) {
	return c.Client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: managedSelector},
		Type:     []hcloud.ImageType{hcloud.ImageTypeSnapshot},
	})
}

func (c *Cloud) AllSnapshots(ctx context.Context) ([]*hcloud.Image, error) {
	return c.Client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		Type: []hcloud.ImageType{hcloud.ImageTypeSnapshot},
	})
}

func sortServers(servers []*hcloud.Server) {
	for i := 0; i < len(servers); i++ {
		for j := i + 1; j < len(servers); j++ {
			if servers[j].Name < servers[i].Name {
				servers[i], servers[j] = servers[j], servers[i]
			}
		}
	}
}
