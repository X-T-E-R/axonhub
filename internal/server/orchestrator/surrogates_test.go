package orchestrator

import (
	"io"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestSurrogatesPassThroughAndFullPassThrough(t *testing.T) {
	const raw = `{"model":"alias","z":9007199254740993,"input":[{"s":"\ud83d...\udef0\ud83d\ud83d\udef0","literal":"\\ud83d","pair":"\uD83D\uDEF0","signature":"opaque","a":1}]}`
	const want = `{"model":"mapped","z":9007199254740993,"input":[{"s":"\ufffd...\ufffd\ufffd\ud83d\udef0","literal":"\\ud83d","pair":"\uD83D\uDEF0","signature":"opaque","a":1}]}`
	for _, format := range []llm.APIFormat{llm.APIFormatOpenAIResponse, llm.APIFormatOpenAIResponseCompact, llm.APIFormatOpenAIChatCompletion, llm.APIFormatAnthropicMessage, llm.APIFormatGeminiContents} {
		for _, full := range []bool{false, true} {
			t.Run(string(format)+"/full="+map[bool]string{true: "true", false: "false"}[full], func(t *testing.T) {
				row := &ent.Channel{Settings: &objects.ChannelSettings{PassThroughBody: lo.ToPtr(true), FullPassThrough: full}, BaseURL: "http://upstream.invalid"}
				if full {
					row.Type = entchannel.TypeAxonhub
				}
				out := &PersistentOutboundTransformer{state: &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{Channel: row}}, LlmRequest: &llm.Request{Model: "mapped", APIFormat: format, RawRequest: &httpclient.Request{Method: http.MethodPost, Path: "/v1/responses", Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(raw)}}}}
				r := &httpclient.Request{Method: http.MethodPost, URL: "http://upstream.invalid/v1/responses", APIFormat: string(format), Headers: http.Header{"Content-Type": {"application/json"}}}
				var err error
				if full {
					r, err = applyAxonHubFullPassThroughRequest(out).OnOutboundRawRequest(t.Context(), r)
				} else {
					r, err = applyPassThroughRequestBody(out, nil).OnOutboundRawRequest(t.Context(), r)
				}
				require.NoError(t, err)
				expected := want
				if !passThroughBodyNeedsModelPatch(format) {
					expected = `{"model":"alias"` + want[len(`{"model":"mapped"`):]
				}
				require.Equal(t, expected, string(r.Body))
				wire, err := httpclient.BuildHttpRequest(t.Context(), r)
				require.NoError(t, err)
				body, err := io.ReadAll(wire.Body)
				require.NoError(t, err)
				require.Equal(t, expected, string(body))
				require.Equal(t, raw, string(out.state.LlmRequest.RawRequest.Body))
			})
		}
	}
}

func TestSurrogateNormalizationLeavesOpaqueVideoMultipartUntouched(t *testing.T) {
	const body = "--test\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\n\"\\ud800\"\r\n--test--\r\n"
	got, err := mergePassThroughRequestBody([]byte(body), llm.APIFormatOpenAIVideo, "mapped")
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}
