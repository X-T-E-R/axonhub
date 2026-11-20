package gemini

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestInboundTransformer_TransformError_RequestBodyTooLarge(t *testing.T) {
	trans := NewInboundTransformer()

	tests := []struct {
		name         string
		err          error
		expectedBody string
	}{
		{
			name:         "wrapped ErrHTTPRequestBodyTooLarge",
			err:          fmt.Errorf("failed to read request body: %w", httpclient.ErrHTTPRequestBodyTooLarge),
			expectedBody: `{"error":{"code":413,"message":"failed to read request body: http request body exceeds size limit","status":"INVALID_ARGUMENT"}}`,
		},
		{
			name:         "MaxBytesError",
			err:          fmt.Errorf("failed to read request body: %w", &http.MaxBytesError{Limit: 32 << 20}),
			expectedBody: `{"error":{"code":413,"message":"failed to read request body: http: request body too large","status":"INVALID_ARGUMENT"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := trans.TransformError(context.Background(), tt.err)
			require.NotNil(t, result)
			require.Equal(t, http.StatusRequestEntityTooLarge, result.StatusCode)
			require.JSONEq(t, tt.expectedBody, string(result.Body))
		})
	}
}
