package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"unsafe"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

// Providers can queue usage after a finish response. Preserve their
// small billing fields when raw input is consumed, even if a caller closes
// before the normalized usage response is dequeued. Reuse the provider's
// aggregator for platform-specific merging; no content/tool deltas are kept.
type streamObservationMeta struct {
	start     *httpclient.StreamEvent
	final     *httpclient.StreamEvent
	chatID    string
	chatUsage *openai.Usage
}

type chatObservationMeta struct {
	Type  string        `json:"type,omitempty"`
	ID    string        `json:"id"`
	Usage *openai.Usage `json:"usage"`
}

type anthropicObservationMeta struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string           `json:"id"`
		Usage *anthropic.Usage `json:"usage,omitempty"`
	} `json:"message,omitempty"`
	Delta *struct {
		StopReason *string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	Usage *anthropic.Usage `json:"usage,omitempty"`
}

func (m *streamObservationMeta) observe(event *httpclient.StreamEvent) {
	if event == nil || len(event.Data) == 0 {
		return
	}
	// Fast byte pre-filter: >99% of streaming events are content/thinking deltas
	// that carry neither billing usage nor lifecycle boundaries.
	data := event.Data
	if !bytes.Contains(data, []byte(`"usage"`)) &&
		!bytes.Contains(data, []byte(`"message_start"`)) &&
		!bytes.Contains(data, []byte(`"message_delta"`)) &&
		!bytes.Contains(data, []byte(`"response.completed"`)) {
		return
	}
	// The aggregator dispatches on JSON type, not the SSE event header.
	dataStr := unsafe.String(unsafe.SliceData(data), len(data))
	kind := gjson.Get(dataStr, "type").String()
	var value any
	var decodedType *string
	switch kind {
	case "response.completed":
		projected := &struct {
			Type     string `json:"type"`
			Response struct {
				ID     string           `json:"id"`
				Status string           `json:"status"`
				Usage  *responses.Usage `json:"usage,omitempty"`
			} `json:"response"`
		}{}
		value, decodedType = projected, &projected.Type
	case "message_start", "message_delta":
		projected := &anthropicObservationMeta{}
		value, decodedType = projected, &projected.Type
	default:
		if kind != "" || !gjson.Get(dataStr, "usage").IsObject() {
			return
		}
		projected := &chatObservationMeta{}
		value, decodedType = projected, &projected.Type
	}
	// Reject ambiguous projections (including duplicate type keys) rather than
	// validating one type and sending another type to the consumer. Raw events
	// remain untouched and still follow the original business decoder.
	if json.Unmarshal(data, value) != nil || *decodedType != kind {
		return
	}
	if start, ok := value.(*anthropicObservationMeta); ok && kind == "message_start" && start.Message == nil {
		// The business decoder ignores absent start messages. Do not feed an
		// invalid start into the optional aggregator, which requires Message.
		return
	}
	if chat, ok := value.(*chatObservationMeta); ok {
		m.chatUsage = chat.Usage
		if chat.ID != "" {
			m.chatID = chat.ID
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	// Use the json.Unmarshal-owned *decodedType string instead of the zero-copy
	// gjson substring so large raw SSE event buffers are never pinned.
	ownedType := *decodedType
	chunk := &httpclient.StreamEvent{Type: ownedType, Data: data}
	if ownedType == "message_start" {
		m.start = chunk
	} else {
		if ownedType == "message_delta" && m.final != nil {
			// Fold earlier usage updates into a small start message, using the
			// existing merge rules. Platform conversion is applied only later
			// by the selected outbound transformer, not to this raw message.
			if body, _, err := anthropic.AggregateStreamChunks(context.Background(), m.chunks(), anthropic.PlatformDirect); err == nil {
				data := append([]byte(`{"type":"message_start","message":`), body...)
				data = append(data, '}')
				m.start = &httpclient.StreamEvent{Type: "message_start", Data: data}
			}
		}
		m.final = chunk
	}
}

func (m *streamObservationMeta) chunks() []*httpclient.StreamEvent {
	if m.final == nil {
		return nil
	}
	if m.start != nil {
		return []*httpclient.StreamEvent{m.start, m.final}
	}
	return []*httpclient.StreamEvent{m.final}
}
