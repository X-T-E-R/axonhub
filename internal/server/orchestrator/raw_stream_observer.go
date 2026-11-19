package orchestrator

import (
	"bytes"
	"unsafe"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// rawStreamObserver performs a single protocol-aware scan per raw SSE event
// shared across direct pass-through performance, rate-limit TPM, and model
// circuit-breaker consumers so each event is parsed at most once.
type rawStreamObserver struct {
	apiFormat llm.APIFormat
	lastEvent *httpclient.StreamEvent

	firstContentSeen bool
	reasoningStarted bool
	reasoningEnded   bool
	terminalSuccess  bool
	terminalFailed   bool

	inputTokens      int64
	outputTokens     int64
	totalTokens      int64
	reportedTotalTPM int64
}

func (s *PersistenceState) ensureRawStreamObserver() *rawStreamObserver {
	if s == nil {
		return &rawStreamObserver{}
	}
	if s.rawStreamObs == nil {
		var format llm.APIFormat
		if s.RawProviderRequest != nil && s.RawProviderRequest.APIFormat != "" {
			format = llm.APIFormat(s.RawProviderRequest.APIFormat)
		} else if s.LlmRequest != nil {
			format = s.LlmRequest.APIFormat
		}
		s.rawStreamObs = &rawStreamObserver{apiFormat: format}
	}
	return s.rawStreamObs
}

func (o *rawStreamObserver) observe(event *httpclient.StreamEvent) {
	if o == nil || event == nil || event == o.lastEvent {
		return
	}
	o.lastEvent = event

	if len(event.Data) == 0 {
		return
	}
	data := bytes.TrimSpace(event.Data)
	if len(data) == 0 {
		return
	}
	if string(data) == "[DONE]" {
		if !o.terminalFailed {
			o.terminalSuccess = true
		}
		return
	}
	if event.Type == "error" {
		o.terminalFailed = true
		return
	}
	if data[0] != '{' {
		return
	}

	s := unsafe.String(unsafe.SliceData(data), len(data))

	switch o.apiFormat {
	case llm.APIFormatAnthropicMessage:
		o.observeAnthropic(event.Type, data, s)
	case llm.APIFormatOpenAIResponse:
		o.observeOpenAIResponses(event.Type, data, s)
	case llm.APIFormatOpenAIChatCompletion:
		o.observeOpenAIChat(data, s)
	default:
		// Infer from event type / payload when format is unset (unit tests).
		evType := event.Type
		if evType == "" {
			evType = gjson.Get(s, "type").Str
		}
		switch {
		case evType == "message_start" || evType == "message_delta" || evType == "message_stop" || evType == "content_block_start" || evType == "content_block_delta" || evType == "ping":
			o.observeAnthropic(evType, data, s)
		case len(evType) >= 9 && evType[:9] == "response.":
			o.observeOpenAIResponses(evType, data, s)
		default:
			o.observeOpenAIChat(data, s)
		}
	}
}

func (o *rawStreamObserver) observeAnthropic(evType string, data []byte, s string) {
	if evType == "" {
		evType = gjson.Get(s, "type").Str
	}
	switch evType {
	case "ping":
		return
	case "error":
		o.terminalFailed = true
		return
	case "message_start":
		if bytes.Contains(data, []byte(`"usage"`)) {
			if u := gjson.Get(s, "message.usage"); u.IsObject() {
				in := u.Get("input_tokens").Int() +
					u.Get("cache_creation_input_tokens").Int() +
					u.Get("cache_read_input_tokens").Int()
				if in > 0 {
					o.inputTokens = in
				}
				if out := u.Get("output_tokens").Int(); out > o.outputTokens {
					o.outputTokens = out
				}
				o.totalTokens = o.inputTokens + o.outputTokens
			}
		}
	case "content_block_start":
		cbType := gjson.Get(s, "content_block.type").Str
		switch cbType {
		case "thinking", "redacted_thinking":
			o.firstContentSeen = true
			o.reasoningStarted = true
		case "tool_use", "server_tool_use":
			o.firstContentSeen = true
			if o.reasoningStarted {
				o.reasoningEnded = true
			}
		case "text":
			if gjson.Get(s, "content_block.text").Str != "" {
				o.firstContentSeen = true
				if o.reasoningStarted {
					o.reasoningEnded = true
				}
			}
		}
	case "content_block_delta":
		d := gjson.Get(s, "delta")
		switch d.Get("type").Str {
		case "thinking_delta", "signature_delta":
			if d.Get("thinking").Str != "" || d.Get("signature").Str != "" {
				o.firstContentSeen = true
				o.reasoningStarted = true
			}
		default:
			if d.Get("text").Str != "" || d.Get("partial_json").Str != "" {
				o.firstContentSeen = true
				if o.reasoningStarted {
					o.reasoningEnded = true
				}
			}
		}
	case "message_delta":
		if sr := gjson.Get(s, "delta.stop_reason"); sr.Type == gjson.String && sr.Str != "" && !o.terminalFailed {
			o.terminalSuccess = true
		}
		if bytes.Contains(data, []byte(`"usage"`)) {
			if u := gjson.Get(s, "usage"); u.IsObject() {
				in := u.Get("input_tokens").Int() +
					u.Get("cache_creation_input_tokens").Int() +
					u.Get("cache_read_input_tokens").Int()
				if in > 0 {
					o.inputTokens = in
				}
				if out := u.Get("output_tokens").Int(); out > 0 {
					o.outputTokens = out
				}
				o.totalTokens = o.inputTokens + o.outputTokens
			}
		}
	case "message_stop":
		if !o.terminalFailed {
			o.terminalSuccess = true
		}
	}
}

func (o *rawStreamObserver) observeOpenAIChat(data []byte, s string) {
	if bytes.Contains(data, []byte(`"error"`)) {
		if errObj := gjson.Get(s, "error"); errObj.IsObject() || (errObj.Type == gjson.String && errObj.Str != "") {
			o.terminalFailed = true
			return
		}
	}
	if choice0 := gjson.Get(s, "choices.0"); choice0.Exists() {
		delta := choice0.Get("delta")
		if delta.Get("reasoning_content").Str != "" || delta.Get("reasoning").Str != "" {
			o.firstContentSeen = true
			o.reasoningStarted = true
		} else if delta.Get("content").Str != "" ||
			delta.Get("refusal").Str != "" ||
			(delta.Get("tool_calls").IsArray() && len(delta.Get("tool_calls").Array()) > 0) {
			o.firstContentSeen = true
			if o.reasoningStarted {
				o.reasoningEnded = true
			}
		}
		if fr := choice0.Get("finish_reason"); fr.Type == gjson.String && fr.Str != "" && !o.terminalFailed {
			o.terminalSuccess = true
		}
	}
	if bytes.Contains(data, []byte(`"usage"`)) {
		if u := gjson.Get(s, "usage"); u.IsObject() {
			pt := u.Get("prompt_tokens").Int()
			ct := u.Get("completion_tokens").Int()
			tt := u.Get("total_tokens").Int()
			if tt == 0 && (pt > 0 || ct > 0) {
				tt = pt + ct
			}
			if pt > 0 {
				o.inputTokens = pt
			}
			if ct > 0 {
				o.outputTokens = ct
			}
			if tt > 0 {
				o.totalTokens = tt
			}
		}
	}
}

func (o *rawStreamObserver) observeOpenAIResponses(evType string, data []byte, s string) {
	if evType == "" {
		evType = gjson.Get(s, "type").Str
	}
	switch evType {
	case "error", "response.failed":
		o.terminalFailed = true
	case "response.incomplete", "response.cancelled":
		// Official non-success terminals: do not latch terminalSuccess.
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		o.firstContentSeen = true
		o.reasoningStarted = true
	case "response.output_text.delta",
		"response.output_text.done",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.refusal.delta":
		o.firstContentSeen = true
		if o.reasoningStarted {
			o.reasoningEnded = true
		}
	case "response.completed":
		if !o.terminalFailed {
			o.terminalSuccess = true
		}
		if bytes.Contains(data, []byte(`"usage"`)) {
			if u := gjson.Get(s, "response.usage"); u.IsObject() {
				in := u.Get("input_tokens").Int()
				out := u.Get("output_tokens").Int()
				tt := u.Get("total_tokens").Int()
				if tt == 0 && (in > 0 || out > 0) {
					tt = in + out
				}
				if in > 0 {
					o.inputTokens = in
				}
				if out > 0 {
					o.outputTokens = out
				}
				if tt > 0 {
					o.totalTokens = tt
				}
			}
		}
	}
}
