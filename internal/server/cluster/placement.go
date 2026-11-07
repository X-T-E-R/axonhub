package cluster

import (
	"maps"
	"sync/atomic"
)

// PlacementSnapshot is an immutable, non-secret effective configuration. A
// version orders hot reloads; it is not a drain, lease or execution generation.
type PlacementSnapshot struct {
	Version int64             `json:"version"`
	Owners  map[string]string `json:"owners"` // stable credential identity -> owner
}

type placementRuntime struct {
	current atomic.Pointer[PlacementSnapshot]
}

func (c Config) WithRuntime() Config {
	if c.runtime == nil {
		c.runtime = &placementRuntime{}
	}
	return c
}

func (c Config) PlacementVersion() int64 {
	if c.runtime != nil {
		if snapshot := c.runtime.current.Load(); snapshot != nil {
			return snapshot.Version
		}
	}
	return 0
}

// Bindings returns a private copy for query predicates and projections.
func (c Config) Bindings() map[int]ChannelBinding {
	bindings := maps.Clone(c.Channels)
	if c.runtime != nil {
		if snapshot := c.runtime.current.Load(); snapshot != nil {
			for id, binding := range bindings {
				if owner, ok := snapshot.Owners[binding.Identity]; ok {
					binding.Owner = owner
					bindings[id] = binding
				}
			}
		}
	}
	return bindings
}

func (c Config) ApplyPlacement(snapshot PlacementSnapshot) {
	next := &PlacementSnapshot{Version: snapshot.Version, Owners: maps.Clone(snapshot.Owners)}
	for {
		previous := c.runtime.current.Load()
		if previous != nil && previous.Version >= snapshot.Version {
			return
		}
		if c.runtime.current.CompareAndSwap(previous, next) {
			return
		}
	}
}

// AtPlacement isolates a versioned view for a single control-plane projection.
func (c Config) AtPlacement(snapshot PlacementSnapshot) Config {
	c.runtime = &placementRuntime{}
	c.ApplyPlacement(snapshot)
	return c
}
