package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
)

func TestValidateSSEKeepAliveSettings(t *testing.T) {
	require.NoError(t, ValidateSSEKeepAliveSettings(nil))
	require.NoError(t, ValidateSSEKeepAliveSettings(&objects.ChannelSSEKeepAlive{}))

	positive := 15
	require.NoError(t, ValidateSSEKeepAliveSettings(&objects.ChannelSSEKeepAlive{
		IntervalSeconds: &positive,
	}))

	zero := 0
	require.EqualError(t, ValidateSSEKeepAliveSettings(&objects.ChannelSSEKeepAlive{
		IntervalSeconds: &zero,
	}), "intervalSeconds must be greater than zero")
}
