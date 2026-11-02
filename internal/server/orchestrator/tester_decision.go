package orchestrator

import (
	"encoding/json"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer/decision"
)

// Native-only channels are tested with their own protocol, never a chat prompt.
// Mixed/legacy channels keep their existing primary chat test.
func nativeDecisionTestRequest(channel *biz.Channel, model string) (*decision.Inbound, []byte) {
	endpoints := channel.ResolveEndpoints()
	if len(endpoints) == 0 {
		return nil, nil
	}
	for _, ep := range endpoints {
		if !llm.IsDecisionAPIFormat(llm.APIFormat(ep.APIFormat)) {
			return nil, nil
		}
	}
	format := llm.APIFormat(endpoints[0].APIFormat)
	body := map[string]any{"model": model}
	if llm.IsSystemOneAPIFormat(format) {
		body["state"] = "AxonHub native decision connection test."
		body["questions"] = map[string]any{"connection": map[string]string{"type": "noul", "instructions": "Does the text describe a connection test?"}}
	} else {
		body["input"] = "AxonHub native decision connection test."
		body["questions"] = []map[string]string{{"type": "predicate", "name": "connection", "instructions": "Does the text describe a connection test?"}}
	}
	raw, _ := json.Marshal(body)
	return decision.NewInbound(format), raw
}
