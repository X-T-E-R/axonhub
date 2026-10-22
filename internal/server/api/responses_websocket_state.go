package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"
)

type responsesWSError struct {
	status                     int
	kind, code, message, param string
	notExecuted                bool
}

func wsRejected(code, message, param string) *responsesWSError {
	return &responsesWSError{status: http.StatusBadRequest, kind: "invalid_request_error", code: code, message: message, param: param, notExecuted: true}
}
func wsPreviousMissing() *responsesWSError {
	return wsRejected("previous_response_not_found", "The previous response is not available on this connection. Send full input without previous_response_id.", "previous_response_id")
}

type responsesWSCreate struct {
	body                        map[string]json.RawMessage
	streamID, previousID, model string
	input                       []json.RawMessage
}

func parseResponsesWSCreate(data []byte) (responsesWSCreate, *responsesWSError) {
	f := responsesWSCreate{}
	if json.Unmarshal(data, &f.body) != nil || f.body == nil {
		return f, wsRejected("invalid_json", "Expected a JSON response.create object.", "")
	}
	if value, ok := f.body["stream_id"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if json.Unmarshal(value, &f.streamID) != nil || f.streamID == "" || len(f.streamID) > 64 || !utf8.ValidString(f.streamID) {
			return f, wsRejected("invalid_stream_id", "stream_id must be a nonempty string of at most 64 bytes.", "stream_id")
		}
	}
	var typ string
	if json.Unmarshal(f.body["type"], &typ) != nil || typ != "response.create" {
		return f, wsRejected("unsupported_event", "Only response.create generation events are supported.", "type")
	}
	for _, key := range []string{"background", "stream", "steering", "conversation", "context_management"} {
		if value, ok := f.body[key]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return f, wsRejected("unsupported_parameter", "This WebSocket endpoint does not support "+key+".", key)
		}
	}
	if value, ok := f.body["generate"]; ok {
		var generate bool
		if json.Unmarshal(value, &generate) != nil || !generate {
			return f, wsRejected("unsupported_parameter", "Only generate:true is supported; warm-up without inference is not available.", "generate")
		}
	}
	if json.Unmarshal(f.body["model"], &f.model) != nil || f.model == "" {
		return f, wsRejected("invalid_request", "model is required.", "model")
	}
	if value, ok := f.body["previous_response_id"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if json.Unmarshal(value, &f.previousID) != nil || f.previousID == "" {
			return f, wsRejected("invalid_request", "previous_response_id must be a nonempty string or null.", "previous_response_id")
		}
	}
	value := f.body["input"]
	if len(value) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if json.Unmarshal(value, &f.input) != nil {
			var text string
			if json.Unmarshal(value, &text) != nil {
				return f, wsRejected("invalid_request", "input must be a string or an array of items.", "input")
			}
			item, _ := json.Marshal(map[string]any{"role": "user", "content": text})
			f.input = []json.RawMessage{item}
		}
	}
	for _, item := range f.input {
		if len(bytes.TrimSpace(item)) == 0 || bytes.TrimSpace(item)[0] != '{' {
			return f, wsRejected("invalid_request", "Each input item must be an object.", "input")
		}
	}
	return f, nil
}

type responsesWSBaseline struct {
	id, model string
	input     []json.RawMessage
	expires   time.Time
	size      int
}
type responsesWSState struct {
	lanes map[string]responsesWSBaseline
	named map[string]struct{}
	ttl   time.Duration
	bytes int
}

func newResponsesWSState(ttl time.Duration) *responsesWSState {
	return &responsesWSState{lanes: map[string]responsesWSBaseline{}, named: map[string]struct{}{}, ttl: ttl}
}
func (s *responsesWSState) clear() { clear(s.lanes); s.bytes = 0 }
func (s *responsesWSState) retire(lane string) {
	if old, ok := s.lanes[lane]; ok {
		s.bytes -= old.size
		delete(s.lanes, lane)
	}
}
func (s *responsesWSState) expire(now time.Time) {
	for lane, base := range s.lanes {
		if !now.Before(base.expires) {
			s.retire(lane)
		}
	}
}
func rawItemsBytes(items []json.RawMessage) int {
	n := 0
	for _, item := range items {
		// Include retained slice-entry overhead as well as JSON bytes.
		n += len(item) + 24
	}
	return n
}
func copyRawItems(items []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i := range items {
		out[i] = append(json.RawMessage(nil), items[i]...)
	}
	return out
}
func (s *responsesWSState) prepare(f responsesWSCreate, now time.Time) ([]byte, []json.RawMessage, *responsesWSError) {
	s.expire(now)
	if f.streamID != "" {
		if _, ok := s.named[f.streamID]; !ok {
			if len(s.named) >= 8 {
				return nil, nil, wsRejected("stream_capacity_exceeded", "This connection supports at most 8 distinct named streams.", "stream_id")
			}
			s.named[f.streamID] = struct{}{}
		}
	}
	input := f.input
	if f.previousID != "" {
		var parent *responsesWSBaseline
		for _, base := range s.lanes {
			if base.id == f.previousID && base.model == f.model {
				parent = &base
				break
			}
		}
		if parent == nil {
			return nil, nil, wsPreviousMissing()
		}
		input = append(copyRawItems(parent.input), f.input...)
	}
	if rawItemsBytes(input) > responsesWSMaxBytes {
		return nil, nil, wsRejected("context_capacity_exceeded", "Expanded input exceeds the connection context limit; send a smaller complete input window.", "input")
	}
	// Current instructions/tools/options are never inherited from a previous frame.
	delete(f.body, "type")
	delete(f.body, "stream_id")
	delete(f.body, "generate")
	delete(f.body, "previous_response_id")
	f.body["input"], _ = json.Marshal(input)
	f.body["stream"] = json.RawMessage("true")
	body, _ := json.Marshal(f.body)
	if len(body) > responsesWSMaxBytes {
		return nil, nil, wsRejected("context_capacity_exceeded", "Expanded request exceeds the connection frame limit.", "input")
	}
	return body, input, nil
}
func (s *responsesWSState) commit(f responsesWSCreate, input []json.RawMessage, completed json.RawMessage, now time.Time) bool {
	var response struct {
		ID     string            `json:"id"`
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(completed, &response) != nil || response.ID == "" || response.Status != "completed" || response.Output == nil {
		return false
	}
	for _, item := range response.Output {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return false
		}
	}
	next := append(copyRawItems(input), response.Output...)
	size := rawItemsBytes(next) + len(response.ID) + len(f.model)
	if size > responsesWSMaxBytes {
		return false
	}
	s.retire(f.streamID)
	// Evict the oldest checkpoint first. Each named lane retains only its latest
	// completed response; stream_id schedules events, previous_response_id is lineage.
	for s.bytes+size > responsesWSMaxBytes {
		oldest := ""
		found := false
		var expiry time.Time
		for lane, base := range s.lanes {
			if !found || base.expires.Before(expiry) {
				oldest = lane
				expiry = base.expires
				found = true
			}
		}
		if !found {
			return false
		}
		s.retire(oldest)
	}
	s.lanes[f.streamID] = responsesWSBaseline{id: response.ID, model: f.model, input: copyRawItems(next), expires: now.Add(s.ttl), size: size}
	s.bytes += size
	return true
}
