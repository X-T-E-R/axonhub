package xcache

import (
	"time"

	"github.com/looplj/axonhub/internal/pkg/xredis"
)

// Mode represents the cache backend mode
//   - memory: pure in-memory
//   - redis: pure redis
//   - two-level: memory + redis chain
const (
	ModeMemory   = "memory"
	ModeRedis    = "redis"
	ModeTwoLevel = "two-level"
)

// Config controls cache backends. Namespace scopes Redis keys shared by
// independent AxonHub instances; an empty namespace uses DefaultNamespace.
// Namespace changes intentionally select a new keyspace and do not fall back to
// legacy unscoped keys.
type Config struct {
	Mode      string        `conf:"mode" yaml:"mode" json:"mode"`
	Namespace string        `conf:"namespace" yaml:"namespace" json:"namespace"`
	Memory    MemoryConfig  `conf:"memory" yaml:"memory" json:"memory"`
	Redis     xredis.Config `conf:"redis" yaml:"redis" json:"redis"`
}

// WatcherChannel scopes live cache notifications to the same application domain.
func (c Config) WatcherChannel(name string) string {
	namespace := c.Namespace
	if namespace == "" {
		namespace = "axonhub"
	}
	return namespace + ":cache:" + name
}

type MemoryConfig struct {
	Expiration      time.Duration `conf:"expiration" yaml:"expiration" json:"expiration"`
	CleanupInterval time.Duration `conf:"cleanup_interval" yaml:"cleanup_interval" json:"cleanup_interval"`
}
