package biz

import (
	"testing"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/stretchr/testify/require"
)

func TestNativeDecisionEndpointDefaultsAndOutbound(t *testing.T) {
	svc := &ChannelService{}
	for _, tc := range []struct {
		name       string
		endpoints  []objects.ChannelEndpoint
		nativeOnly bool
	}{
		{"legacy", nil, false},
		{"native_only", []objects.ChannelEndpoint{{APIFormat: "typesafe/systemone", Path: "/custom/systemone"}}, true},
		{"mixed", []objects.ChannelEndpoint{{APIFormat: "typesafe/systemone"}, {APIFormat: "openai/chat_completions"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &ent.Channel{ID: 1, Name: tc.name, Type: channel.TypeOpenai, BaseURL: "https://synthetic.invalid", Credentials: objects.ChannelCredentials{APIKey: "synthetic"}, Endpoints: tc.endpoints}
			built, err := svc.buildChannelWithOutbounds(row)
			require.NoError(t, err)
			if tc.nativeOnly {
				require.Empty(t, DefaultEndpointsForChannel(row))
				require.Len(t, built.Outbounds, 1)
				require.Equal(t, llm.APIFormatTypeSafeSystemOne, built.Outbound.APIFormat())
				require.Equal(t, tc.endpoints, built.ResolveEndpoints())
			} else {
				require.NotEmpty(t, DefaultEndpointsForChannel(row))
				require.Contains(t, built.Outbounds, llm.APIFormatOpenAIChatCompletion.String())
				require.Contains(t, built.Outbounds, llm.APIFormatOpenAIEmbedding.String())
				require.Equal(t, llm.APIFormatOpenAIChatCompletion, built.Outbound.APIFormat())
			}
		})
	}
}
