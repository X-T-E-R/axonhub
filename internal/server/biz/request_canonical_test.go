package biz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalJSONUsesJCSMemberOrderAndStringEscaping(t *testing.T) {
	canonical, err := canonicalJSON(map[string]any{
		"b": int64(1),
		"a": "<profile>&",
	})
	require.NoError(t, err)
	require.Equal(t, `{"a":"<profile>&","b":1}`, string(canonical))
}
