// Package cluster defines server-controlled execution placement. An empty Config
// retains the standalone contract; client headers never select a channel or owner.
package cluster

import (
	"fmt"
	"net/url"
)

type ChannelBinding struct {
	Owner string `conf:"owner" yaml:"owner" json:"owner"`
	// Identity is the administrator's stable upstream account identifier. Channels
	// sharing credentials, including rotated OAuth credentials, use the same identity.
	Identity string `conf:"identity" yaml:"identity" json:"identity"`
}

type Config struct {
	Enabled     bool                   `conf:"enabled" yaml:"enabled" json:"enabled"`
	NodeID      string                 `conf:"node_id" yaml:"node_id" json:"node_id"`
	ControlNode string                 `conf:"control_node" yaml:"control_node" json:"control_node"`
	Nodes       map[string]string      `conf:"nodes" yaml:"nodes" json:"nodes"`
	Channels    map[int]ChannelBinding `conf:"channels" yaml:"channels" json:"channels"`
	runtime     *placementRuntime
}

func (c Config) IsControl() bool { return !c.Enabled || c.NodeID == c.ControlNode }
func (c Config) Owner(id int) string {
	if c.runtime != nil {
		if snapshot := c.runtime.current.Load(); snapshot != nil {
			if owner, ok := snapshot.Owners[c.Channels[id].Identity]; ok {
				return owner
			}
		}
	}
	return c.Channels[id].Owner
}
func (c Config) Owns(id int) bool { return !c.Enabled || c.Owner(id) == c.NodeID }
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.NodeID == "" || c.ControlNode == "" {
		return fmt.Errorf("cluster requires node_id and control_node")
	}
	if _, ok := c.Nodes[c.NodeID]; !ok {
		return fmt.Errorf("cluster node_id is absent from nodes")
	}
	if _, ok := c.Nodes[c.ControlNode]; !ok {
		return fmt.Errorf("cluster control_node is absent from nodes")
	}
	for node, origin := range c.Nodes {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("invalid cluster origin for node %q", node)
		}
	}
	identities := map[string]string{}
	for id, b := range c.Channels {
		if id <= 0 || b.Identity == "" {
			return fmt.Errorf("cluster channel %d requires a stable identity", id)
		}
		if _, ok := c.Nodes[b.Owner]; !ok {
			return fmt.Errorf("cluster channel %d has unknown owner %q", id, b.Owner)
		}
		if owner, ok := identities[b.Identity]; ok && owner != b.Owner {
			return fmt.Errorf("cluster identity %q has more than one owner", b.Identity)
		}
		identities[b.Identity] = b.Owner
	}
	return nil
}

// GlobalTask identifies maintenance that must run only on the control node.
// Local consumers and channel-owned work remain registered on every node.
func GlobalTask(name string) bool {
	switch name {
	case "backup", "gc", "managed-observability-capacity-gc", "channel-probe":
		return true
	default:
		return false
	}
}
