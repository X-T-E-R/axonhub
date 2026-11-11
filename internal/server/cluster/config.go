// Package cluster defines server-controlled execution placement. An empty Config
// retains the standalone contract; client headers never select a channel or owner.
package cluster

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type ChannelBinding struct {
	Owner string `conf:"owner" yaml:"owner" json:"owner"`
	// Identity is the administrator's stable upstream account identifier. Channels
	// sharing credentials, including rotated OAuth credentials, use the same identity.
	Identity string `conf:"identity" yaml:"identity" json:"identity"`
}

const (
	defaultCompactSyncInterval   = 5 * time.Minute
	defaultCompactMaxBatchCount  = 256
	defaultCompactMaxBatchBytes  = 256 << 10
	defaultCompactMaxSpoolCount  = 65536
	defaultCompactMaxSpoolBytes  = 64 << 20
	defaultCompactRequestTimeout = 15 * time.Second
)

// CompactAccountingConfig configures local SQLite outbox persistence and
// low-frequency idempotent batch uploads for non-control data nodes.
type CompactAccountingConfig struct {
	Enabled        bool          `conf:"enabled" yaml:"enabled" json:"enabled"`
	OutboxPath     string        `conf:"outbox_path" yaml:"outbox_path" json:"outbox_path"`
	SyncInterval   time.Duration `conf:"sync_interval" yaml:"sync_interval" json:"sync_interval"`
	MaxBatchCount  int           `conf:"max_batch_count" yaml:"max_batch_count" json:"max_batch_count"`
	MaxBatchBytes  int           `conf:"max_batch_bytes" yaml:"max_batch_bytes" json:"max_batch_bytes"`
	MaxSpoolCount  int           `conf:"max_spool_records" yaml:"max_spool_records" json:"max_spool_records"`
	MaxSpoolBytes  int64         `conf:"max_spool_bytes" yaml:"max_spool_bytes" json:"max_spool_bytes"`
	RequestTimeout time.Duration `conf:"request_timeout" yaml:"request_timeout" json:"request_timeout"`
	SharedSecret   string        `conf:"shared_secret" yaml:"shared_secret" json:"shared_secret"`
}

func (c CompactAccountingConfig) WithDefaults(nodeID string) CompactAccountingConfig {
	if strings.TrimSpace(c.OutboxPath) == "" {
		suffix := strings.TrimSpace(nodeID)
		if suffix == "" {
			suffix = "node"
		}
		c.OutboxPath = fmt.Sprintf("axonhub-compact-outbox-%s.db", suffix)
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = defaultCompactSyncInterval
	}
	if c.MaxBatchCount <= 0 {
		c.MaxBatchCount = defaultCompactMaxBatchCount
	}
	if c.MaxBatchBytes <= 0 {
		c.MaxBatchBytes = defaultCompactMaxBatchBytes
	}
	if c.MaxSpoolCount <= 0 {
		c.MaxSpoolCount = defaultCompactMaxSpoolCount
	}
	if c.MaxSpoolBytes <= 0 {
		c.MaxSpoolBytes = defaultCompactMaxSpoolBytes
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultCompactRequestTimeout
	}
	return c
}

type Config struct {
	Enabled           bool                    `conf:"enabled" yaml:"enabled" json:"enabled"`
	NodeID            string                  `conf:"node_id" yaml:"node_id" json:"node_id"`
	ControlNode       string                  `conf:"control_node" yaml:"control_node" json:"control_node"`
	Nodes             map[string]string       `conf:"nodes" yaml:"nodes" json:"nodes"`
	Channels          map[int]ChannelBinding  `conf:"channels" yaml:"channels" json:"channels"`
	CompactAccounting CompactAccountingConfig `conf:"compact_accounting" yaml:"compact_accounting" json:"compact_accounting"`
	runtime           *placementRuntime
}

func (c Config) IsControl() bool              { return !c.Enabled || c.NodeID == c.ControlNode }
func (c Config) CompactAccountingActive() bool { return c.Enabled && !c.IsControl() && c.CompactAccounting.Enabled }
func (c Config) Owner(id int) string          { return c.Binding(id).Owner }
func (c Config) Owns(id int) bool             { return !c.Enabled || c.Owner(id) == c.NodeID }
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
	if c.CompactAccounting.Enabled {
		if c.CompactAccounting.SyncInterval < 0 {
			return fmt.Errorf("cluster.compact_accounting.sync_interval cannot be negative")
		}
		if c.CompactAccounting.MaxBatchCount < 0 || c.CompactAccounting.MaxBatchBytes < 0 ||
			c.CompactAccounting.MaxSpoolCount < 0 || c.CompactAccounting.MaxSpoolBytes < 0 ||
			c.CompactAccounting.RequestTimeout < 0 {
			return fmt.Errorf("cluster.compact_accounting limits cannot be negative")
		}
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
