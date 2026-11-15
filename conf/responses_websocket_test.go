package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketFrameLimit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   int64
	}{
		{"default", "", 983040},
		{"yaml", "server:\n  responses_websocket_max_frame_bytes: 524288\n", 524288},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := loadConfig(writeTestConfig(t, tc.config))
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.APIServer.ResponsesWebSocketMaxFrameBytes)
		})
	}
	t.Run("environment overrides yaml", func(t *testing.T) {
		t.Setenv("AXONHUB_SERVER_RESPONSES_WEBSOCKET_MAX_FRAME_BYTES", "65536")
		cfg, _, err := loadConfig(writeTestConfig(t, "server:\n  responses_websocket_max_frame_bytes: 524288\n"))
		require.NoError(t, err)
		require.EqualValues(t, 65536, cfg.APIServer.ResponsesWebSocketMaxFrameBytes)
	})
	for _, value := range []string{"0", "-1", "33554433"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			_, _, err := loadConfig(writeTestConfig(t, "server:\n  responses_websocket_max_frame_bytes: "+value+"\n"))
			require.ErrorContains(t, err, "responses_websocket_max_frame_bytes")
		})
	}
}
