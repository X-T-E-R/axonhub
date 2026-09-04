package orchestrator

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestErrorResponseBodyForStoragePreservesRawBytes(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body []byte
	}{
		{name: "JSON formatting", body: []byte("{\n  \"error\": \"synthetic-placeholder\"\n}\n")},
		{name: "credential-shaped JSON field", body: []byte(`{"access_token":"synthetic-placeholder"}`)},
		{name: "plain text", body: []byte("upstream error: Bearer synthetic-placeholder")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stored := errorResponseBodyForStorage(testCase.body)

			require.Equal(t, testCase.body, stored)
			require.NotSame(t, &testCase.body[0], &stored[0])
		})
	}
}

func TestErrorResponseBodyForStorageUsesTransportBound(t *testing.T) {
	body := bytes.Repeat([]byte("x"), httpclient.MaxErrorBodySize+4096)
	stored := errorResponseBodyForStorage(body)

	require.Len(t, stored, httpclient.MaxErrorBodySize)
	require.Equal(t, body[:httpclient.MaxErrorBodySize], stored)
}
