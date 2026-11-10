package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketStreamParameter(t *testing.T) {
	for _, tc := range []struct {
		name, field, code string
	}{
		{name: "omitted"},
		{name: "true", field: `,"stream":true`},
		{name: "null", field: `,"stream":null`},
		{name: "false", field: `,"stream":false`, code: "unsupported_parameter"},
		{name: "string_true", field: `,"stream":"true"`, code: "invalid_request"},
		{name: "string_false", field: `,"stream":"false"`, code: "invalid_request"},
		{name: "number", field: `,"stream":1`, code: "invalid_request"},
		{name: "zero", field: `,"stream":0`, code: "invalid_request"},
		{name: "object", field: `,"stream":{}`, code: "invalid_request"},
		{name: "array", field: `,"stream":[]`, code: "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, rejection := parseResponsesWSCreate([]byte(fmt.Sprintf(`{"type":"response.create","model":"m","input":"offline"%s}`, tc.field)))
			if tc.code != "" {
				require.NotNil(t, rejection)
				require.Equal(t, http.StatusBadRequest, rejection.status)
				require.Equal(t, "invalid_request_error", rejection.kind)
				require.Equal(t, tc.code, rejection.code)
				require.Equal(t, "stream", rejection.param)
				require.True(t, rejection.notExecuted)
				return
			}
			require.Nil(t, rejection)
			body, _, rejection := newResponsesWSState(time.Minute).prepare(frame, time.Now())
			require.Nil(t, rejection)
			var prepared map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &prepared))
			require.Equal(t, "true", string(prepared["stream"]))
		})
	}
}
