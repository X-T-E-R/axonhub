package biz

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestSerializeObservationRequest(t *testing.T) {
	t.Run("raw body takes precedence and headers are masked", func(t *testing.T) {
		req := &httpclient.Request{
			JSONBody: []byte(`{"model":"raw"}`),
			Body:     []byte("ignored"),
			Headers:  http.Header{"Authorization": {"Bearer private"}, "Content-Type": {"application/json"}},
		}
		body, headers, err := serializeObservationRequest(t.Context(), req, true)
		require.NoError(t, err)
		require.Equal(t, string(req.JSONBody), string(body))
		require.Contains(t, string(headers), "application/json")
		require.NotContains(t, string(headers), "private")
	})
	t.Run("byte body keeps existing raw encoding", func(t *testing.T) {
		body, headers, err := serializeObservationRequest(t.Context(), &httpclient.Request{Body: []byte(`{"model":"bytes"}`)}, true)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"bytes"}`, string(body))
		require.JSONEq(t, `{}`, string(headers))
	})
	t.Run("omission does not serialize body or headers", func(t *testing.T) {
		body, headers, err := serializeObservationRequest(t.Context(), &httpclient.Request{Body: []byte("omitted"), Headers: http.Header{"Authorization": {"private"}}}, false)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(body))
		require.JSONEq(t, `{}`, string(headers))
	})
}

func TestDetachedObservationContextRetainsOnlyAllowlistedValues(t *testing.T) {
	type unrelatedKey struct{}
	ctx := context.WithValue(t.Context(), unrelatedKey{}, "pipeline state")
	capturedAt := time.Now().UTC()
	body := &requestObservationBodyInfo{Unavailable: true, ByteLength: 17, SHA256: "request"}
	response := &requestObservationBodyInfo{Unavailable: true, ByteLength: 23, SHA256: "response"}
	pending := &observationPendingUsage{apiKeyID: 7, createdAt: capturedAt}
	ctx = withObservationCreatedAt(ctx, capturedAt)
	ctx = withObservationBodyInfo(ctx, body)
	ctx = withObservationResponseBodyInfo(ctx, response)
	ctx = withObservationID(ctx, "observation-nonce")
	ctx = context.WithValue(ctx, requestObservationProfilesKey{}, true)
	ctx = context.WithValue(ctx, observationPendingUsageKey{}, pending)
	detached := detachedObservationContext(ctx)
	gotTime, ok := observationCreatedAt(detached)
	require.True(t, ok)
	require.Equal(t, capturedAt, gotTime)
	require.Equal(t, body, observationBodyInfoFromContext(detached))
	require.Equal(t, response, observationResponseBodyInfoFromContext(detached))
	require.Equal(t, "observation-nonce", observationIDFromContext(detached))
	require.Equal(t, true, detached.Value(requestObservationProfilesKey{}))
	require.Same(t, pending, detached.Value(observationPendingUsageKey{}))
	require.Nil(t, detached.Value(unrelatedKey{}))
}

func TestDeferredObservationUnrecognizedKeyKeepsSynchronousPath(t *testing.T) {
	svc := &DataStorageService{}
	ctx := context.WithValue(t.Context(), observationPersistenceKey{}, observationPersistenceContext{writer: &ForwardingObservationWriter{}})
	deferred, err := svc.deferExternalObservation(ctx, nil, "unrelated-object-key", []byte("body"))
	require.NoError(t, err)
	require.False(t, deferred)
}
