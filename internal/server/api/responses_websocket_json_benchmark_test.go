package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

// Run with -run '^$' -bench '^BenchmarkResponsesWebSocketJSON$' -benchmem.
// This exercises frame parsing, continuation preparation and the real inbound
// transformer without a WebSocket peer, database, or provider.
func BenchmarkResponsesWebSocketJSON(b *testing.B) {
	const text = `workspace diff: status=ready; continue with the requested change. 路径 <src> \"name\" \\ \uD83D\uDEF0. `
	message := `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat(text, 96) + `"}]}`
	input := `[` + strings.Repeat(message+`,`, 47) + message + `]`
	full := []byte(`{"type":"response.create","model":"codex","input":` + input + `}`)
	raw := []byte(`{"type":"response.create","model":"codex","input":` + strings.TrimSuffix(input, "]") + `,{"type":"tool_search_call","call_id":"call_1","arguments":{"query":"synthetic"},"opaque":{"value":9007199254740993}}]}`)
	delta := []byte(`{"type":"response.create","model":"codex","previous_response_id":"resp_1","input":[` + message + `]}`)

	b.Run("InputUnion", func(b *testing.B) {
		data := []byte(input)
		b.ReportAllocs()
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			var in responses.Input
			if err := in.UnmarshalJSON(data); err != nil {
				b.Fatal(err)
			}
		}
	})

	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{name: "Full", frame: full},
		{name: "FullRawExtension", frame: raw},
		{name: "Continuation", frame: delta},
	} {
		b.Run(tc.name, func(b *testing.B) {
			state := newResponsesWSState(time.Minute)
			now := time.Unix(0, 0)
			if tc.name == "Continuation" {
				frame, err := parseResponsesWSCreate(full)
				if err != nil {
					b.Fatal(err.message)
				}
				_, items, err := state.prepare(frame, now)
				if err != nil {
					b.Fatal(err.message)
				}
				completed := json.RawMessage(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}]}`)
				if !state.commit(frame, items, completed, now) {
					b.Fatal("failed to seed continuation")
				}
			}
			transformer := responses.NewInboundTransformer()
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				frame, err := parseResponsesWSCreate(tc.frame)
				if err != nil {
					b.Fatal(err.message)
				}
				body, _, err := state.prepare(frame, now)
				if err != nil {
					b.Fatal(err.message)
				}
				if _, err := transformer.TransformRequest(ctx, &httpclient.Request{Body: body}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
