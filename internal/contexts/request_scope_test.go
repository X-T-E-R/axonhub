package contexts

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/stretchr/testify/require"
)

func TestNewRequestScopeIsolatesMutableGenerationState(t *testing.T) {
	parent := WithAPIKey(context.Background(), &ent.APIKey{ID: 1})
	parent = WithProjectID(parent, 2)
	parent = WithTraceID(parent, "trace")
	parent = WithRequestID(parent, "handshake")
	parent = WithChannelAPIKey(parent, "upstream-secret")
	AddError(parent, errors.New("old"))
	a, b := NewRequestScope(parent), NewRequestScope(parent)
	_, ok := GetAPIKey(a)
	require.False(t, ok)
	_, ok = GetProjectID(a)
	require.False(t, ok)
	_, ok = GetChannelAPIKey(a)
	require.False(t, ok)
	require.Empty(t, GetErrors(a))
	trace, _ := GetTraceID(a)
	require.Equal(t, "trace", trace)
	var wg sync.WaitGroup
	for _, ctx := range []context.Context{a, b} {
		wg.Go(func() {
			for range 100 {
				WithRequestID(ctx, "frame")
				WithChannelAPIKey(ctx, "frame-key")
				AddError(ctx, errors.New("frame"))
			}
		})
	}
	wg.Wait()
	id, _ := GetRequestID(parent)
	require.Equal(t, "handshake", id)
	require.Len(t, GetErrors(parent), 1)
	require.NotSame(t, getContainer(a), getContainer(b))
}
