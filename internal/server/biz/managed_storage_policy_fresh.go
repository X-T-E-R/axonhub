package biz

import (
	"context"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

// StoragePolicyFresh bypasses the instance-local cached policy. The caller must
// select the primary when using a read/write split client.
// Reuse StoragePolicy so legacy field defaults remain identical.
func (s *SystemService) StoragePolicyFresh(ctx context.Context) (*StoragePolicy, error) {
	reader := &SystemService{AbstractService: s.AbstractService, Cache: xcache.NewNoop[ent.System]()}
	return reader.StoragePolicy(ctx)
}
