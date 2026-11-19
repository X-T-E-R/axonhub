package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

type runtimeStats struct {
	HeapAlloc    uint64  `json:"heapAllocBytes"`
	HeapInuse    uint64  `json:"heapInuseBytes"`
	HeapObjects  uint64  `json:"heapObjects"`
	TotalAlloc   uint64  `json:"totalAllocBytes"`
	NumGC        uint32  `json:"numGC"`
	PauseTotalNs uint64  `json:"gcPauseTotalNs"`
	GCCPU        float64 `json:"gcCPUFractionSinceStart"`
	Goroutines   int     `json:"goroutines"`
}

type sample struct {
	VmRSSKiB     uint64  `json:"vm_rss_kib"`
	VmHWMKiB     uint64  `json:"vm_hwm_kib"`
	HeapAlloc    uint64  `json:"heap_alloc_bytes"`
	HeapInuse    uint64  `json:"heap_inuse_bytes"`
	TotalAlloc   uint64  `json:"total_alloc_bytes"`
	NumGC        uint32  `json:"num_gc"`
	PauseTotalNs uint64  `json:"pause_total_ns"`
	GCCPU        float64 `json:"gc_cpu_fraction"`
	Goroutines   int     `json:"goroutines"`
}

func readProcMem(pid int) (rssKiB, hwmKiB uint64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0
	}
	for _, ln := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		if k == "VmRSS" || k == "VmHWM" {
			fields := strings.Fields(v)
			if len(fields) > 0 {
				val, _ := strconv.ParseUint(fields[0], 10, 64)
				if k == "VmRSS" {
					rssKiB = val
				} else {
					hwmKiB = val
				}
			}
		}
	}
	return rssKiB, hwmKiB
}

func fetchRuntimeStats(client *http.Client, debugURL string) runtimeStats {
	resp, err := client.Get(debugURL + "/debug/runtime")
	if err != nil {
		return runtimeStats{}
	}
	defer resp.Body.Close()
	var s runtimeStats
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func captureSample(client *http.Client, pid int, debugURL string) sample {
	rss, hwm := readProcMem(pid)
	rt := fetchRuntimeStats(client, debugURL)
	return sample{
		VmRSSKiB:     rss,
		VmHWMKiB:     hwm,
		HeapAlloc:    rt.HeapAlloc,
		HeapInuse:    rt.HeapInuse,
		TotalAlloc:   rt.TotalAlloc,
		NumGC:        rt.NumGC,
		PauseTotalNs: rt.PauseTotalNs,
		GCCPU:        rt.GCCPU,
		Goroutines:   rt.Goroutines,
	}
}

func saveHeapProfile(client *http.Client, debugURL, outPath string) error {
	resp, err := client.Get(debugURL + "/debug/pprof/heap?gc=1")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pprof heap HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("empty pprof heap profile from %s", debugURL)
	}
	return os.WriteFile(outPath, data, 0o644)
}

func mustSaveHeapProfile(client *http.Client, debugURL, outPath string) {
	if err := saveHeapProfile(client, debugURL, outPath); err != nil {
		panic(fmt.Errorf("saveHeapProfile(%s): %w", outPath, err))
	}
}

// --- Synthetic real-topology payload builders ---

func stressCode(seed, n int) string {
	if n <= 0 {
		return ""
	}
	rng := rand.New(rand.NewPCG(uint64(seed), 42))
	var b strings.Builder
	b.Grow(n + 128)
	for b.Len() < n {
		fmt.Fprintf(&b, "func validateRecord%04x(row Record, limits Limits) error { if row.Value > limits.Max { return fmt.Errorf(`record %08x rejected in rev %04x`) }; return nil }\n",
			rng.Uint32N(1<<16), rng.Uint32(), rng.Uint32N(1<<16))
	}
	return b.String()[:n]
}

func buildTools() []map[string]any {
	tools := make([]map[string]any, 55)
	for i := range tools {
		fields := map[string]any{
			"target": map[string]any{"type": "object", "required": []string{"kind", "key"}, "properties": map[string]any{
				"kind": map[string]any{"type": "string", "enum": []string{"queue", "policy", "catalog", "ledger"}},
				"key":  map[string]any{"type": "string", "maxLength": 128, "description": "Synthetic record identifier"},
			}},
			"filters": map[string]any{"type": "array", "maxItems": 20, "items": map[string]any{"type": "object", "required": []string{"field", "condition"}, "properties": map[string]any{
				"field":     map[string]any{"type": "string", "enum": []string{"revision", "status", "scope", "rollout"}},
				"condition": map[string]any{"anyOf": []map[string]any{{"type": "string"}, {"type": "integer"}, {"type": "null"}}},
			}}},
			"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
			"include_graph": map[string]any{"type": "boolean", "default": false},
		}
		tools[i] = map[string]any{
			"name":        fmt.Sprintf("inspect_record_%02d", i),
			"description": "Read the synthetic change record and verify its revision and projected fields.",
			"input_schema": map[string]any{
				"type":                 "object",
				"required":             []string{"target"},
				"properties":           fields,
				"additionalProperties": false,
			},
		}
	}
	return tools
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func buildAnthropicMessages(n, slot, seedOffset int) []map[string]any {
	msgs := make([]map[string]any, n)
	for i := range msgs {
		id := fmt.Sprintf("toolu_syn_%03d", i/2)
		if i%2 == 0 {
			content := []map[string]any{{"type": "text", "text": "Review the generated revision and verify invariants."}}
			if i > 0 {
				content = append(content, map[string]any{
					"type":        "tool_result",
					"tool_use_id": fmt.Sprintf("toolu_syn_%03d", i/2-1),
					"content":     []map[string]any{{"type": "text", "text": "```go\n" + stressCode(i+seedOffset, slot) + "\n```"}},
				})
			}
			msgs[i] = map[string]any{"role": "user", "content": content}
		} else {
			msgs[i] = map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "thinking", "thinking": "Check the change record and inspect its projected fields before proceeding.", "signature": "synthetic_signature_dGhpc19pc19ub3RfcmVhbA=="},
				{"type": "tool_use", "id": id, "name": "inspect_record_00", "input": map[string]any{"target": map[string]any{"kind": "ledger", "key": "snapshot"}, "limit": 10}},
			}}
		}
	}
	msgs[n-1] = map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "Summarize the verified synthetic changes."}}}
	return msgs
}

func buildAnthropicBodySeeded(model string, targetBytes, seedOffset int) []byte {
	const n = 121
	req := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 65536,
		"metadata":   map[string]any{"user_id": "usr_crucible"},
		"system":     "Work only with the synthetic records supplied in this benchmark.",
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 32768},
		"tools":      buildTools(),
		"messages":   buildAnthropicMessages(n, 0, seedOffset),
	}
	const slots = n/2 - 1
	blankSize := len(mustJSON(req))
	req["messages"] = buildAnthropicMessages(n, max(0, (targetBytes-blankSize)*99/100/slots), seedOffset)
	b := mustJSON(req)
	if len(b) < targetBytes {
		b = append(b, bytes.Repeat([]byte(" "), targetBytes-len(b))...)
	}
	return b
}

func buildAnthropicBody(model string, targetBytes int) []byte {
	return buildAnthropicBodySeeded(model, targetBytes, 0)
}

func buildOpenAIBody(model, mode string, targetBytes int) []byte {
	const turns = 45
	var items []map[string]any
	var tools []map[string]any
	for _, x := range buildTools() {
		f := map[string]any{"name": x["name"], "description": x["description"], "parameters": x["input_schema"]}
		if mode == "chat" {
			f = map[string]any{"type": "function", "function": f}
		} else {
			f["type"] = "function"
		}
		tools = append(tools, f)
	}
	slot := max(0, (targetBytes-75_000)*99/100/turns)
	for i := range turns {
		id := fmt.Sprintf("call_syn_%03d", i)
		args := `{"target":{"kind":"ledger","key":"snapshot"}}`
		code := "```go\n" + stressCode(i, slot) + "\n```"
		items = append(items, map[string]any{"role": "user", "content": "Review the generated change record."})
		if mode == "chat" {
			items = append(items,
				map[string]any{"role": "assistant", "tool_calls": []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "inspect_record_00", "arguments": args}}}},
				map[string]any{"role": "tool", "tool_call_id": id, "content": code},
			)
		} else {
			items = append(items,
				map[string]any{"type": "function_call", "name": "inspect_record_00", "call_id": id, "arguments": args},
				map[string]any{"type": "function_call_output", "call_id": id, "output": code},
			)
		}
	}
	items = append(items, map[string]any{"role": "user", "content": "Summarize the changes."})
	req := map[string]any{
		"model":    model,
		"stream":   true,
		"tools":    tools,
		"metadata": map[string]any{"user_id": "usr_crucible"},
	}
	if mode == "chat" {
		req["messages"] = items
		req["max_tokens"] = 65536
		req["stream_options"] = map[string]any{"include_usage": true}
	} else {
		req["input"] = items
		req["max_output_tokens"] = 65536
	}
	b := mustJSON(req)
	if len(b) < targetBytes {
		b = append(b, bytes.Repeat([]byte(" "), targetBytes-len(b))...)
	}
	return b
}

func buildAnthropicSSE(model string, n int) []string {
	frame := func(kind, payload string) string { return "event: " + kind + "\ndata: " + payload + "\n\n" }
	delta := func(index int, payload string) string {
		return frame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, index, payload))
	}
	events := []string{
		frame("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_bench_0001","type":"message","role":"assistant","model":%q,"content":[],"usage":{"input_tokens":450000,"output_tokens":1,"cache_read_input_tokens":12000,"cache_creation_input_tokens":500}}}`, model)),
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
	}
	thinking := delta(0, `{"type":"thinking_delta","thinking":"Inspect the synthetic revision and validate query boundaries. "}`)
	for range n / 4 {
		events = append(events, thinking)
	}
	events = append(events,
		delta(0, `{"type":"signature_delta","signature":"synthetic_signature_Zml4dHVyZQ=="}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
	)
	text := delta(1, `{"type":"text_delta","text":"The synthetic record retains its revision and scope cleanly. "}`)
	for len(events) < n-4 {
		events = append(events, text)
	}
	events = append(events,
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`),
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2000}}`),
		frame("message_stop", `{"type":"message_stop"}`),
	)
	return events
}

func buildOpenAIChatSSE(model string, n int) []string {
	events := make([]string, 0, n+2)
	events = append(events, fmt.Sprintf("data: {\"id\":\"chatcmpl_bench_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n", model))
	chunk := fmt.Sprintf("data: {\"id\":\"chatcmpl_bench_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Synthetic streaming output token chunk. \"},\"finish_reason\":null}]}\n\n", model)
	for len(events) < n-2 {
		events = append(events, chunk)
	}
	events = append(events,
		fmt.Sprintf("data: {\"id\":\"chatcmpl_bench_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":380000,\"completion_tokens\":1500,\"total_tokens\":381500}}\n\n", model),
		"data: [DONE]\n\n",
	)
	return events
}

func buildResponsesSSE(model string, n int) []string {
	events := []string{
		fmt.Sprintf("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_bench_1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":%q,\"output\":[]}}\n\n", model),
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n",
	}
	for i := 2; i < n-2; i++ {
		events = append(events, fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":%d,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Synthetic responses token chunk. \"}\n\n", i))
	}
	events = append(events,
		fmt.Sprintf("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":%d,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}}\n\n", n-2),
		fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":%d,\"response\":{\"id\":\"resp_bench_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":%q,\"output\":[],\"usage\":{\"input_tokens\":380000,\"output_tokens\":1500,\"total_tokens\":381500}}}\n\n", n-1, model),
	)
	return events
}

// --- Active Wire-Verifying Mock Upstream SSE Server ---

type mockUpstream struct {
	anthFrames           []string
	chatFrames           []string
	respFrames           []string
	spikeFrame           string
	holdGate             chan struct{}
	midGate              chan struct{}
	openedCh             chan struct{}
	midCh                chan struct{}
	totalReqs            atomic.Int64
	wireVerifiedCnt      atomic.Int64
	wireMismatchCnt      atomic.Int64
	threadContinueMatCnt atomic.Int64
	firstMismatch        atomic.Value
	retries502           atomic.Int64
	retriesPreErr        atomic.Int64
	enable502            atomic.Bool
	enablePreErr         atomic.Bool
	enableHold           atomic.Bool
	verifyWire           atomic.Bool
	expectThreads        atomic.Bool
	spikeNext            atomic.Bool
	canceledCnt          atomic.Int64
	bufPool              sync.Pool
}

func newMockUpstream(anth, chat, resp []string, spike string) *mockUpstream {
	return &mockUpstream{
		anthFrames: anth,
		chatFrames: chat,
		respFrames: resp,
		spikeFrame: spike,
		holdGate:   make(chan struct{}),
		midGate:    make(chan struct{}),
		openedCh:   make(chan struct{}, 2048),
		midCh:      make(chan struct{}, 2048),
		bufPool: sync.Pool{
			New: func() any { return bytes.NewBuffer(make([]byte, 0, 64<<10)) },
		},
	}
}

func (m *mockUpstream) validateWire(r *http.Request, body []byte) error {
	isBackup := strings.HasPrefix(r.URL.Path, "/backup/")
	tier := "primary"
	suffix := "-pri"
	if isBackup {
		tier = "backup"
		suffix = "-bkp"
	}

	var baseModel string
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		baseModel = "bench-anthropic"
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		baseModel = "bench-chat"
	case strings.HasSuffix(r.URL.Path, "/responses"):
		baseModel = "bench-responses"
	default:
		return nil
	}
	wantModel := baseModel + suffix

	gotModel := gjson.GetBytes(body, "model").String()
	if gotModel != wantModel {
		return fmt.Errorf("wire model mismatch on %s: got %q want %q", r.URL.Path, gotModel, wantModel)
	}
	gotTier := gjson.GetBytes(body, "crucible_tier").String()
	if gotTier != tier {
		return fmt.Errorf("wire body override crucible_tier mismatch on %s: got %q want %q", r.URL.Path, gotTier, tier)
	}
	gotUser := gjson.GetBytes(body, "crucible_user").String()
	if gotUser != "usr_crucible" {
		return fmt.Errorf("wire body template crucible_user mismatch on %s: got %q want usr_crucible", r.URL.Path, gotUser)
	}
	wantRouteHdr := tier + "-" + wantModel
	if got := r.Header.Get("X-Crucible-Route"); got != wantRouteHdr {
		return fmt.Errorf("wire header X-Crucible-Route mismatch on %s: got %q want %q", r.URL.Path, got, wantRouteHdr)
	}
	if got := r.Header.Get("X-Crucible-User"); got != "usr_crucible" {
		return fmt.Errorf("wire header X-Crucible-User mismatch on %s: got %q want usr_crucible", r.URL.Path, got)
	}
	if m.expectThreads.Load() {
		if gjson.GetBytes(body, "thread").Exists() {
			return fmt.Errorf("wire thread field leaked to upstream on %s", r.URL.Path)
		}
		if strings.Contains(r.Header.Get("Anthropic-Beta"), "message-threads-2026-08-12") {
			return fmt.Errorf("wire message-threads beta token leaked to upstream on %s", r.URL.Path)
		}
		if msgCount := len(gjson.GetBytes(body, "messages").Array()); msgCount >= 3 {
			m.threadContinueMatCnt.Add(1)
		}
	}
	return nil
}

func (m *mockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.verifyWire.Load() {
		buf := m.bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		_, err := io.Copy(buf, r.Body)
		if err != nil {
			if buf.Cap() <= 4<<20 {
				m.bufPool.Put(buf)
			}
			return
		}
		vErr := m.validateWire(r, buf.Bytes())
		if buf.Cap() <= 4<<20 {
			m.bufPool.Put(buf)
		}
		if vErr != nil {
			m.wireMismatchCnt.Add(1)
			m.firstMismatch.CompareAndSwap(nil, vErr.Error())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "invalid_request_error", "message": vErr.Error()},
			})
			return
		}
		m.wireVerifiedCnt.Add(1)
	} else {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
	}

	id := m.totalReqs.Add(1)
	isBackup := strings.HasPrefix(r.URL.Path, "/backup/")
	isAnthropic := strings.HasSuffix(r.URL.Path, "/messages")

	if !isBackup {
		if m.enable502.Load() && id%20 == 0 {
			m.retries502.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"synthetic 502 retry"}}`)
			return
		}
		if m.enablePreErr.Load() && isAnthropic && id%33 == 11 {
			m.retriesPreErr.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_pre_err\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"bench-anthropic-pri\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"synthetic pre-content overload\"}}\n\n")
			flusher.Flush()
			return
		}
	}

	var frames []string
	switch {
	case isAnthropic:
		frames = m.anthFrames
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		frames = m.chatFrames
	case strings.HasSuffix(r.URL.Path, "/responses"):
		frames = m.respFrames
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher := w.(http.Flusher)

	injectSpike := m.spikeNext.Swap(false)
	hold := m.enableHold.Load()

	for i, fr := range frames {
		if i == 0 && isAnthropic {
			fr = strings.Replace(fr, "msg_bench_0001", fmt.Sprintf("msg_bench_%08d", id), 1)
		}
		if _, err := io.WriteString(w, fr); err != nil {
			m.canceledCnt.Add(1)
			return
		}
		if injectSpike && i == 3 && m.spikeFrame != "" {
			if _, err := io.WriteString(w, m.spikeFrame); err != nil {
				m.canceledCnt.Add(1)
				return
			}
		}
		if i == 2 || i%64 == 63 || i == len(frames)-1 {
			flusher.Flush()
		}
		if hold {
			if i == 2 {
				select {
				case m.openedCh <- struct{}{}:
				default:
				}
				select {
				case <-m.holdGate:
				case <-r.Context().Done():
					m.canceledCnt.Add(1)
					return
				}
			} else if i == len(frames)/2 {
				flusher.Flush()
				select {
				case m.midCh <- struct{}{}:
				default:
				}
				select {
				case <-m.midGate:
				case <-r.Context().Done():
					m.canceledCnt.Add(1)
					return
				}
			}
		}
	}
}

// --- AxonHub Bootstrap Helpers ---

func postJSON(client *http.Client, url, bearer string, body any) ([]byte, int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

func gql(client *http.Client, baseURL, token, query string, vars map[string]any) (map[string]any, error) {
	payload := map[string]any{"query": query, "variables": vars}
	data, code, err := postJSON(client, baseURL+"/admin/graphql", token, payload)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("graphql HTTP %d: %s", code, string(data))
	}
	var out struct {
		Data   map[string]any `json:"data"`
		Errors []any          `json:"errors"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode gql: %w (%s)", err, string(data))
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %v", out.Errors)
	}
	return out.Data, nil
}

func bootstrapAxonHub(client *http.Client, baseURL, upstreamURL string, passThrough, verifyWire bool, keyCount int) ([]string, []int, error) {
	deadline := time.Now().Add(45 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/health")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			ready = true
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !ready {
		return nil, nil, fmt.Errorf("axonhub /health did not become ready in time")
	}

	// 1. Initialize system
	data, code, err := postJSON(client, baseURL+"/admin/system/initialize", "", map[string]any{
		"ownerEmail":     "bench@example.invalid",
		"ownerPassword":  "BenchPass123!",
		"ownerFirstName": "Bench",
		"ownerLastName":  "Owner",
		"brandName":      "AxonBench",
	})
	if err != nil || code != 200 {
		return nil, nil, fmt.Errorf("init failed (%d): %v %s", code, err, string(data))
	}

	// 2. Sign in
	data, code, err = postJSON(client, baseURL+"/admin/auth/signin", "", map[string]any{
		"email":    "bench@example.invalid",
		"password": "BenchPass123!",
	})
	if err != nil || code != 200 {
		return nil, nil, fmt.Errorf("signin failed (%d): %v %s", code, err, string(data))
	}
	var signIn struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &signIn); err != nil || signIn.Token == "" {
		return nil, nil, fmt.Errorf("signin missing token: %s", string(data))
	}
	tok := signIn.Token

	// 3. StoragePolicy all-false (default lean production mode)
	_, err = gql(client, baseURL, tok, `mutation {
		updateStoragePolicy(input: {
			storeRequestBody: false
			storeExecutionRequestBody: false
			storeResponseBody: false
			storeChunks: false
			livePreview: false
		})
	}`, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("updateStoragePolicy: %w", err)
	}

	// 4. RetryPolicy: enable channel failover retry
	_, err = gql(client, baseURL, tok, `mutation {
		updateRetryPolicy(input: {
			enabled: true
			maxChannelRetries: 1
			maxSingleChannelRetries: 0
			retryDelayMs: 5
			loadBalancerStrategy: "failover"
		})
	}`, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("updateRetryPolicy: %w", err)
	}

	// 5. Create Channels with distinct primary/backup model mappings & body/header override templates
	channels := []struct {
		chType string
		name   string
		url    string
		model  string
		mapped string
		tier   string
		weight int
	}{
		{"anthropic", "anth-primary", upstreamURL, "bench-anthropic", "bench-anthropic-pri", "primary", 100},
		{"anthropic", "anth-backup", upstreamURL + "/backup", "bench-anthropic", "bench-anthropic-bkp", "backup", 10},
		{"openai", "chat-primary", upstreamURL, "bench-chat", "bench-chat-pri", "primary", 100},
		{"openai", "chat-backup", upstreamURL + "/backup", "bench-chat", "bench-chat-bkp", "backup", 10},
		{"openai_responses", "resp-primary", upstreamURL, "bench-responses", "bench-responses-pri", "primary", 100},
		{"openai_responses", "resp-backup", upstreamURL + "/backup", "bench-responses", "bench-responses-bkp", "backup", 10},
	}
	var chIDs []string
	for _, ch := range channels {
		settings := map[string]any{
			"passThroughBody": passThrough,
			"proxy":           map[string]any{"type": "disabled"},
		}
		supported := []string{ch.model}
		defaultTest := ch.model
		if verifyWire {
			// Only list the mapped target model in supportedModels so AxonHub applies
			// modelMappings ("bench-anthropic" -> "bench-anthropic-pri"/"-bkp") instead
			// of matching the unmapped alias directly.
			supported = []string{ch.mapped}
			defaultTest = ch.mapped
			settings["modelMappings"] = []map[string]any{
				{"from": ch.model, "to": ch.mapped},
			}
			settings["bodyOverrideOperations"] = []map[string]any{
				{"op": "set", "path": "crucible_tier", "value": ch.tier},
				{"op": "set", "path": "crucible_user", "value": `{{index .Metadata "user_id"}}`},
			}
			settings["headerOverrideOperations"] = []map[string]any{
				{"op": "set", "path": "X-Crucible-Route", "value": ch.tier + "-{{.Model}}"},
				{"op": "set", "path": "X-Crucible-User", "value": `{{index .Metadata "user_id"}}`},
			}
		}
		res, err := gql(client, baseURL, tok, `mutation CreateChannel($input: CreateChannelInput!) {
			createChannel(input: $input) { id }
		}`, map[string]any{
			"input": map[string]any{
				"type":             ch.chType,
				"name":             ch.name,
				"baseURL":          ch.url,
				"credentials":      map[string]any{"apiKey": "synthetic-upstream-key-" + ch.name},
				"supportedModels":  supported,
				"defaultTestModel": defaultTest,
				"orderingWeight":   ch.weight,
				"settings":         settings,
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("createChannel %s: %w", ch.name, err)
		}
		if cc, ok := res["createChannel"].(map[string]any); ok {
			if id, ok := cc["id"].(string); ok && id != "" {
				chIDs = append(chIDs, id)
			}
		}
	}
	if len(chIDs) > 0 {
		if _, err := gql(client, baseURL, tok, `mutation BulkEnableChannels($ids: [ID!]!) {
			bulkEnableChannels(ids: $ids)
		}`, map[string]any{"ids": chIDs}); err != nil {
			return nil, nil, fmt.Errorf("bulkEnableChannels: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	var numericChIDs []int
	for _, rawID := range chIDs {
		s := rawID
		if idx := strings.LastIndex(s, "/"); idx >= 0 {
			s = s[idx+1:]
		}
		if n, convErr := strconv.Atoi(s); convErr == nil && n > 0 {
			numericChIDs = append(numericChIDs, n)
		}
	}

	// 6. Query default project ID & create API Key(s)
	projData, err := gql(client, baseURL, tok, `query { projects(first: 5) { edges { node { id name } } } }`, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("query projects: %w", err)
	}
	projID := ""
	if projects, ok := projData["projects"].(map[string]any); ok {
		if edges, ok := projects["edges"].([]any); ok && len(edges) > 0 {
			if node, ok := edges[0].(map[string]any)["node"].(map[string]any); ok {
				projID, _ = node["id"].(string)
			}
		}
	}
	if projID == "" {
		return nil, nil, fmt.Errorf("no default project found: %v", projData)
	}

	if keyCount < 1 {
		keyCount = 1
	}
	apiKeys := make([]string, 0, keyCount)
	for k := range keyCount {
		keyData, err := gql(client, baseURL, tok, `mutation CreateAPIKey($input: CreateAPIKeyInput!) {
			createAPIKey(input: $input) { id key }
		}`, map[string]any{
			"input": map[string]any{
				"name":      fmt.Sprintf("bench-key-%d", k),
				"type":      "service_account",
				"scopes":    []string{"read_channels", "write_requests"},
				"projectID": projID,
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("createAPIKey #%d: %w", k, err)
		}
		apiKey := ""
		if ck, ok := keyData["createAPIKey"].(map[string]any); ok {
			apiKey, _ = ck["key"].(string)
		}
		if apiKey == "" {
			return nil, nil, fmt.Errorf("empty apiKey #%d: %v", k, keyData)
		}
		apiKeys = append(apiKeys, apiKey)
	}
	return apiKeys, numericChIDs, nil
}

func drainSSE(resp *http.Response, abort bool, slow bool) (int, string, error) {
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 8192), 16<<20)
	lines := 0
	terminal := false
	msgID := ""
	for sc.Scan() {
		lines++
		ln := sc.Bytes()
		if msgID == "" && bytes.HasPrefix(ln, []byte("data: ")) && bytes.Contains(ln, []byte(`"message_start"`)) {
			if id := gjson.GetBytes(ln[6:], "message.id").String(); id != "" {
				msgID = id
			}
		}
		if bytes.Contains(ln, []byte("message_stop")) || bytes.Contains(ln, []byte("[DONE]")) || bytes.Contains(ln, []byte("response.completed")) {
			terminal = true
		}
		if abort && lines >= 30 {
			return lines, msgID, nil
		}
		if slow && lines%120 == 100 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if err := sc.Err(); err != nil {
		return lines, msgID, err
	}
	if !terminal {
		return lines, msgID, fmt.Errorf("stream ended without terminal event (lines=%d)", lines)
	}
	return lines, msgID, nil
}

func main() {
	sutBin := flag.String("sut", "", "path to axonhub binary")
	scenario := flag.String("scenario", "anthropic_plain", "anthropic_plain|anthropic_zstd|anthropic_256|anthropic_chaos|anthropic_threads|openai_chat|openai_responses|gomemlimit_220|dcz|dcz_multi|compact_accounting")
	conc := flag.Int("concurrency", 128, "concurrent streams")
	bodyMiB := flag.Float64("body-mib", 2.5, "request body size in MiB")
	events := flag.Int("events", 2000, "SSE events per stream")
	holdSec := flag.Int("hold-sec", 5, "seconds to hold all streams open mid-stream")
	memLimit := flag.String("gomemlimit", "", "GOMEMLIMIT env for axonhub")
	outDir := flag.String("out", "/tmp/bench-out", "output directory for json & pprof")
	passThrough := flag.Bool("passthrough", true, "channel passThroughBody setting")
	verifyWire := flag.Bool("verify-wire", true, "strictly validate model remap + body/header override templates on upstream (HTTP 422 on mismatch)")
	injectPreErr := flag.Bool("inject-precontent-error", false, "inject pre-content ping->message_start->error failover on primary")
	pgDSN := flag.String("pg-dsn", "", "optional PostgreSQL DSN for AxonHub (defaults to isolated SQLite)")
	redisURL := flag.String("redis-url", "", "optional Redis URL for AxonHub cache mode")
	flag.Parse()

	if *sutBin == "" {
		fmt.Fprintln(os.Stderr, "--sut is required")
		os.Exit(2)
	}
	_ = os.MkdirAll(*outDir, 0o755)

	bodyBytes := int(*bodyMiB * 1024 * 1024)

	// 1. Start mock upstream
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	up := newMockUpstream(
		buildAnthropicSSE("bench-anthropic", *events),
		buildOpenAIChatSSE("bench-chat", *events),
		buildResponsesSSE("bench-responses", *events),
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\""+strings.Repeat("S", 4<<20)+"\"}}\n\n",
	)
	up.verifyWire.Store(*verifyWire)
	upSrv := &http.Server{Handler: up}
	go upSrv.Serve(upLn)
	defer upSrv.Close()
	upstreamURL := "http://" + upLn.Addr().String()

	// Pick free ports for AxonHub SUT, debug listener, and (for compact_accounting) Control Node A.
	pickPort := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		return p
	}
	sutPort := pickPort()
	dbgPort := pickPort()

	workDir, _ := os.MkdirTemp("", "axon-sut-*")
	defer os.RemoveAll(workDir)
	dbPath := filepath.Join(workDir, "axonhub.db")

	sutBase := fmt.Sprintf("http://127.0.0.1:%d", sutPort)
	dbgBase := fmt.Sprintf("http://127.0.0.1:%d", dbgPort)
	adminClient := &http.Client{Timeout: 30 * time.Second}

	keyCount := 1
	if *scenario == "dcz_multi" {
		// Server retains up to 4 dictionaries per API key scope; 6 API keys x 4 dicts = 24 active
		// dictionaries (> maxCachedDictDecoders=16), exercising real cross-tenant decoder LRU eviction.
		keyCount = 6
	}

	var (
		sutCmd     *exec.Cmd
		sutLog     *os.File
		apiKeys    []string
		outboxPath string
	)

	if *scenario == "compact_accounting" {
		if *pgDSN == "" || *redisURL == "" {
			panic("compact_accounting requires --pg-dsn and --redis-url")
		}
		ctrlPort := pickPort()
		ctrlBase := fmt.Sprintf("http://127.0.0.1:%d", ctrlPort)
		ctrlDir, _ := os.MkdirTemp("", "axon-ctrl-*")
		defer os.RemoveAll(ctrlDir)

		ctrlYAML := fmt.Sprintf(`server:
  host: "127.0.0.1"
  port: %d
db:
  dialect: "postgres"
  dsn: %q
cache:
  mode: "redis"
  redis:
    url: %q
log:
  level: "error"
  output: "stdio"
cluster:
  enabled: true
  node_id: "ctrl-a"
  control_node: "ctrl-a"
  nodes:
    ctrl-a: %q
    data-b: %q
  channels:
    1:
      owner: "data-b"
      identity: "crucible-acct-1"
    2:
      owner: "data-b"
      identity: "crucible-acct-2"
    3:
      owner: "data-b"
      identity: "crucible-acct-3"
    4:
      owner: "data-b"
      identity: "crucible-acct-4"
    5:
      owner: "data-b"
      identity: "crucible-acct-5"
    6:
      owner: "data-b"
      identity: "crucible-acct-6"
    7:
      owner: "data-b"
      identity: "crucible-acct-7"
    8:
      owner: "data-b"
      identity: "crucible-acct-8"
  compact_accounting:
    enabled: true
    shared_secret: "crucible-compact-secret"
`, ctrlPort, *pgDSN, *redisURL, ctrlBase, sutBase)
		if err := os.WriteFile(filepath.Join(ctrlDir, "config.yml"), []byte(ctrlYAML), 0o600); err != nil {
			panic(err)
		}

		ctrlCmd := exec.Command(*sutBin)
		ctrlCmd.Dir = ctrlDir
		ctrlCmd.Env = os.Environ()
		ctrlLog, _ := os.Create(filepath.Join(*outDir, "ctrl.log"))
		ctrlCmd.Stdout = ctrlLog
		ctrlCmd.Stderr = ctrlLog
		if err := ctrlCmd.Start(); err != nil {
			panic(fmt.Errorf("start ctrl node: %w", err))
		}
		defer func() {
			_ = ctrlCmd.Process.Kill()
			_ = ctrlCmd.Wait()
			ctrlLog.Close()
		}()

		var chIDs []int
		apiKeys, chIDs, err = bootstrapAxonHub(adminClient, ctrlBase, upstreamURL, *passThrough, *verifyWire, keyCount)
		if err != nil {
			panic(fmt.Errorf("bootstrap control node: %w", err))
		}
		if len(chIDs) == 0 {
			panic("bootstrap returned zero channel IDs for cluster bindings")
		}

		var chBindings strings.Builder
		for _, id := range chIDs {
			fmt.Fprintf(&chBindings, "    %d:\n      owner: \"data-b\"\n      identity: \"crucible-acct-%d\"\n", id, id)
		}
		outboxPath = filepath.Join(workDir, "compact-outbox-data-b.db")
		dataYAML := fmt.Sprintf(`server:
  host: "127.0.0.1"
  port: %d
  debug_listener: "127.0.0.1:%d"
db:
  dialect: "postgres"
  dsn: %q
  disable_auto_migration: true
cache:
  mode: "redis"
  redis:
    url: %q
log:
  level: "error"
  output: "stdio"
cluster:
  enabled: true
  node_id: "data-b"
  control_node: "ctrl-a"
  nodes:
    ctrl-a: %q
    data-b: %q
  channels:
%s  compact_accounting:
    enabled: true
    outbox_path: %q
    sync_interval: "1s"
    shared_secret: "crucible-compact-secret"
`, sutPort, dbgPort, *pgDSN, *redisURL, ctrlBase, sutBase, chBindings.String(), outboxPath)
		if err := os.WriteFile(filepath.Join(workDir, "config.yml"), []byte(dataYAML), 0o600); err != nil {
			panic(err)
		}

		sutCmd = exec.Command(*sutBin)
		sutCmd.Dir = workDir
		sutEnv := os.Environ()
		if *memLimit != "" {
			sutEnv = append(sutEnv, "GOMEMLIMIT="+*memLimit)
		}
		sutCmd.Env = sutEnv
		sutLog, _ = os.Create(filepath.Join(*outDir, "sut.log"))
		sutCmd.Stdout = sutLog
		sutCmd.Stderr = sutLog
		if err := sutCmd.Start(); err != nil {
			panic(fmt.Errorf("start data-b sut: %w", err))
		}
		defer func() {
			_ = sutCmd.Process.Kill()
			_ = sutCmd.Wait()
			sutLog.Close()
		}()

		// Wait for Data Node B /health
		deadline := time.Now().Add(45 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			resp, hErr := adminClient.Get(sutBase + "/health")
			if hErr == nil && resp.StatusCode == 200 {
				resp.Body.Close()
				ready = true
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(150 * time.Millisecond)
		}
		if !ready {
			panic("data-b /health did not become ready in time")
		}
	} else {
		sutCmd = exec.Command(*sutBin)
		sutCmd.Dir = workDir
		sutEnv := append(os.Environ(),
			"AXONHUB_SERVER_HOST=127.0.0.1",
			fmt.Sprintf("AXONHUB_SERVER_PORT=%d", sutPort),
			fmt.Sprintf("AXONHUB_SERVER_DEBUG_LISTENER=127.0.0.1:%d", dbgPort),
			"AXONHUB_LOG_LEVEL=error",
			"AXONHUB_LOG_OUTPUT=stdio",
		)
		if *pgDSN != "" {
			sutEnv = append(sutEnv,
				"AXONHUB_DB_DIALECT=postgres",
				"AXONHUB_DB_DSN="+*pgDSN,
			)
		} else {
			sutEnv = append(sutEnv,
				"AXONHUB_DB_DIALECT=sqlite3",
				fmt.Sprintf("AXONHUB_DB_DSN=file:%s?cache=shared&_fk=1&_journal_mode=WAL&_busy_timeout=5000", dbPath),
			)
		}
		if *redisURL != "" {
			sutEnv = append(sutEnv,
				"AXONHUB_CACHE_MODE=redis",
				"AXONHUB_CACHE_REDIS_URL="+*redisURL,
			)
		}
		if *memLimit != "" {
			sutEnv = append(sutEnv, "GOMEMLIMIT="+*memLimit)
		}
		sutCmd.Env = sutEnv
		sutLog, _ = os.Create(filepath.Join(*outDir, "sut.log"))
		sutCmd.Stdout = sutLog
		sutCmd.Stderr = sutLog
		if err := sutCmd.Start(); err != nil {
			panic(fmt.Errorf("start sut: %w", err))
		}
		defer func() {
			_ = sutCmd.Process.Kill()
			_ = sutCmd.Wait()
			sutLog.Close()
		}()

		apiKeys, _, err = bootstrapAxonHub(adminClient, sutBase, upstreamURL, *passThrough, *verifyWire, keyCount)
		if err != nil {
			panic(fmt.Errorf("bootstrap: %w", err))
		}
	}
	apiKey := apiKeys[0]

	// Build request payload(s)
	type reqSpec struct {
		body   []byte
		apiKey string
	}
	var targetURL string
	var reqSpecs []reqSpec
	reqHeaders := http.Header{
		"Content-Type": {"application/json"},
	}

	switch *scenario {
	case "anthropic_plain", "anthropic_256", "anthropic_chaos", "gomemlimit_220", "compact_accounting":
		targetURL = sutBase + "/v1/messages"
		reqSpecs = []reqSpec{{body: buildAnthropicBody("bench-anthropic", bodyBytes), apiKey: apiKey}}
	case "anthropic_threads":
		targetURL = sutBase + "/v1/messages"
		reqHeaders.Set("Anthropic-Beta", "message-threads-2026-08-12")
		up.expectThreads.Store(true)
		// Build a real message-threads Turn-1 create body carrying "thread":{"type":"create"}.
		// Turn-2 continuations carrying "thread":{"type":"continue","previous_message_id":...}
		// are constructed per worker after Turn-1 returns each unique message_start.message.id.
		createObj := map[string]any{
			"model":      "bench-anthropic",
			"stream":     true,
			"max_tokens": 65536,
			"metadata":   map[string]any{"user_id": "usr_crucible"},
			"system":     "Work only with the synthetic records supplied in this benchmark.",
			"thinking":   map[string]any{"type": "enabled", "budget_tokens": 32768},
			"tools":      buildTools()[:10],
			"messages":   []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Initial thread context:\n" + stressCode(7, min(bodyBytes, 128<<10))}}}},
			"thread":     map[string]any{"type": "create"},
		}
		reqSpecs = []reqSpec{{body: mustJSON(createObj), apiKey: apiKey}}
	case "anthropic_zstd":
		targetURL = sutBase + "/v1/messages"
		plain := buildAnthropicBody("bench-anthropic", bodyBytes)
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		reqSpecs = []reqSpec{{body: enc.EncodeAll(plain, nil), apiKey: apiKey}}
		enc.Close()
		reqHeaders.Set("Content-Encoding", "zstd")
	case "dcz", "dcz_multi":
		targetURL = sutBase + "/v1/messages"
		dictsPerKey := 1
		if *scenario == "dcz_multi" {
			dictsPerKey = 4
		}
		plain := buildAnthropicBody("bench-anthropic", bodyBytes)
		encPlain, _ := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
		dIdx := 0
		for _, k := range apiKeys {
			for range dictsPerKey {
				dict := buildAnthropicBodySeeded("bench-anthropic", 256<<10, dIdx*1000+7)
				warmReq, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(encPlain.EncodeAll(dict, nil)))
				warmReq.Header = reqHeaders.Clone()
				warmReq.Header.Set("X-API-Key", k)
				warmReq.Header.Set("Authorization", "Bearer "+k)
				warmReq.Header.Set("Content-Encoding", "zstd")
				warmReq.Header.Set("X-Dict-Store", "1")
				warmResp, err := adminClient.Do(warmReq)
				if err != nil || warmResp.StatusCode != 200 {
					panic(fmt.Errorf("dcz warm #%d failed: %v", dIdx, err))
				}
				_, _ = io.Copy(io.Discard, warmResp.Body)
				warmResp.Body.Close()
				h := sha256.Sum256(dict)
				if warmResp.Header.Get("X-Dict-Stored") != hex.EncodeToString(h[:]) {
					panic(fmt.Errorf("dcz dict #%d not stored: got %q", dIdx, warmResp.Header.Get("X-Dict-Stored")))
				}
				encDict, _ := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, dict), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
				wire := append([]byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0, 0, 0}, h[:]...)
				reqSpecs = append(reqSpecs, reqSpec{body: encDict.EncodeAll(plain, wire), apiKey: k})
				encDict.Close()
				dIdx++
			}
		}
		encPlain.Close()
		reqHeaders.Set("Content-Encoding", "dcz")
	case "openai_chat":
		targetURL = sutBase + "/v1/chat/completions"
		reqSpecs = []reqSpec{{body: buildOpenAIBody("bench-chat", "chat", bodyBytes), apiKey: apiKey}}
	case "openai_responses":
		targetURL = sutBase + "/v1/responses"
		reqSpecs = []reqSpec{{body: buildOpenAIBody("bench-responses", "responses", bodyBytes), apiKey: apiKey}}
	default:
		panic("unknown scenario: " + *scenario)
	}

	// Warm up 1 request before sampling baseline
	{
		wReq, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(reqSpecs[0].body))
		wReq.Header = reqHeaders.Clone()
		wReq.Header.Set("X-API-Key", reqSpecs[0].apiKey)
		wReq.Header.Set("Authorization", "Bearer "+reqSpecs[0].apiKey)
		wResp, err := adminClient.Do(wReq)
		if err != nil {
			panic(fmt.Errorf("warmup failed: %w", err))
		}
		if _, _, err := drainSSE(wResp, false, false); err != nil {
			panic(fmt.Errorf("warmup drain failed: %w", err))
		}
	}

	// For anthropic_threads, create *conc real threads (Turn 1) before activating the mid-stream
	// barrier, then drive all *conc concurrent Turn-2 continuations simultaneously so
	// anthropic_threads.go materialization is genuinely exercised under concurrency.
	var threadContinueBodies [][]byte
	if *scenario == "anthropic_threads" {
		threadContinueBodies = make([][]byte, *conc)
		var twg sync.WaitGroup
		var tErr atomic.Value
		for i := 0; i < *conc; i++ {
			twg.Add(1)
			go func(idx int) {
				defer twg.Done()
				cReq, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(reqSpecs[0].body))
				cReq.Header = reqHeaders.Clone()
				cReq.Header.Set("X-API-Key", apiKey)
				cReq.Header.Set("Authorization", "Bearer "+apiKey)
				cResp, err := adminClient.Do(cReq)
				if err != nil {
					tErr.CompareAndSwap(nil, fmt.Errorf("thread create #%d: %w", idx, err))
					return
				}
				_, msgID, err := drainSSE(cResp, false, false)
				if err != nil || msgID == "" {
					tErr.CompareAndSwap(nil, fmt.Errorf("thread create #%d drain/id=%q: %v", idx, msgID, err))
					return
				}
				contObj := map[string]any{
					"model":      "bench-anthropic",
					"stream":     true,
					"max_tokens": 65536,
					"metadata":   map[string]any{"user_id": "usr_crucible"},
					"thinking":   map[string]any{"type": "enabled", "budget_tokens": 32768},
					"messages":   []map[string]any{{"role": "user", "content": fmt.Sprintf("Turn 2 continuation #%d: summarize the prior thread state.", idx)}},
					"thread":     map[string]any{"type": "continue", "previous_message_id": msgID},
				}
				threadContinueBodies[idx] = mustJSON(contObj)
			}(i)
		}
		twg.Wait()
		if v := tErr.Load(); v != nil {
			panic(v)
		}
	}

	// Reset counters & capture before-load baseline
	up.totalReqs.Store(0)
	up.wireVerifiedCnt.Store(0)
	up.threadContinueMatCnt.Store(0)
	up.retries502.Store(0)
	up.retriesPreErr.Store(0)
	up.canceledCnt.Store(0)
	up.enable502.Store(true)
	up.enablePreErr.Store(*injectPreErr || *scenario == "anthropic_chaos")
	up.enableHold.Store(true)
	if *scenario == "anthropic_256" || *scenario == "anthropic_chaos" {
		up.spikeNext.Store(true)
	}

	mustSaveHeapProfile(adminClient, dbgBase, filepath.Join(*outDir, "before_heap.pb.gz"))
	before := captureSample(adminClient, sutCmd.Process.Pid, dbgBase)

	// Start background peak monitor
	stopMon := make(chan struct{})
	peakCh := make(chan sample, 1)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		peak := before
		for {
			select {
			case <-stopMon:
				peakCh <- peak
				return
			case <-ticker.C:
				s := captureSample(adminClient, sutCmd.Process.Pid, dbgBase)
				if s.VmRSSKiB > peak.VmRSSKiB {
					peak.VmRSSKiB = s.VmRSSKiB
				}
				if s.VmHWMKiB > peak.VmHWMKiB {
					peak.VmHWMKiB = s.VmHWMKiB
				}
				if s.HeapInuse > peak.HeapInuse {
					peak.HeapInuse = s.HeapInuse
				}
				if s.HeapAlloc > peak.HeapAlloc {
					peak.HeapAlloc = s.HeapAlloc
				}
				if s.Goroutines > peak.Goroutines {
					peak.Goroutines = s.Goroutines
				}
			}
		}
	}()

	loadClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *conc * 2,
			MaxIdleConnsPerHost: *conc * 2,
			DisableCompression:  true,
		},
		Timeout: 3 * time.Minute,
	}

	start := time.Now()
	var wg sync.WaitGroup
	var okCnt, abortCnt, errCnt atomic.Int64
	var firstErr atomic.Value

	// For DCZ, stage admission at 4 concurrent decodes (server limit) then hold streams open
	admitCap := *conc
	if *scenario == "dcz" || *scenario == "dcz_multi" {
		admitCap = 4
	}
	admit := make(chan struct{}, admitCap)

	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			admit <- struct{}{}
			spec := reqSpecs[idx%len(reqSpecs)]
			body := spec.body
			if *scenario == "anthropic_threads" {
				body = threadContinueBodies[idx]
			}
			req, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
			req.Header = reqHeaders.Clone()
			req.Header.Set("X-API-Key", spec.apiKey)
			req.Header.Set("Authorization", "Bearer "+spec.apiKey)
			resp, err := loadClient.Do(req)
			<-admit
			if err != nil {
				errCnt.Add(1)
				firstErr.CompareAndSwap(nil, err.Error())
				return
			}
			abort := idx%10 == 9
			slow := idx%7 == 6
			_, _, err = drainSSE(resp, abort, slow)
			if err != nil {
				errCnt.Add(1)
				firstErr.CompareAndSwap(nil, err.Error())
				return
			}
			if abort {
				abortCnt.Add(1)
			} else {
				okCnt.Add(1)
			}
		}(i)
	}

	waitOpen := func(ch <-chan struct{}, n int, d time.Duration) int {
		timer := time.NewTimer(d)
		defer timer.Stop()
		got := 0
		for got < n {
			select {
			case <-ch:
				got++
			case <-timer.C:
				return got
			}
		}
		return got
	}

	openedStreams := waitOpen(up.openedCh, *conc, 60*time.Second)
	if openedStreams < *conc {
		panic(fmt.Errorf("open barrier incomplete: got %d/%d", openedStreams, *conc))
	}
	time.Sleep(1 * time.Second)
	mustSaveHeapProfile(adminClient, dbgBase, filepath.Join(*outDir, "open_heap.pb.gz"))
	openSample := captureSample(adminClient, sutCmd.Process.Pid, dbgBase)

	close(up.holdGate)
	wantMid := *conc - (*conc / 10)
	midStreams := waitOpen(up.midCh, wantMid, 60*time.Second)
	if midStreams < wantMid {
		panic(fmt.Errorf("midstream barrier incomplete: got %d/%d", midStreams, wantMid))
	}
	time.Sleep(time.Duration(*holdSec) * time.Second)

	mustSaveHeapProfile(adminClient, dbgBase, filepath.Join(*outDir, "midstream_heap.pb.gz"))
	midSample := captureSample(adminClient, sutCmd.Process.Pid, dbgBase)

	close(up.midGate)
	wg.Wait()
	elapsed := time.Since(start)
	// Capture main-wave allocation BEFORE optional post-spike wave so alloc_bytes_per_req is uncontaminated.
	mainWaveAfter := captureSample(adminClient, sutCmd.Process.Pid, dbgBase)

	var postSpikeSample sample
	if *scenario == "anthropic_256" || *scenario == "anthropic_chaos" {
		up.enableHold.Store(false)
		up.enable502.Store(false)
		up.enablePreErr.Store(false)
		var wg2 sync.WaitGroup
		smallBody := buildAnthropicBody("bench-anthropic", 8<<10)
		for range 32 {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				req, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(smallBody))
				req.Header = reqHeaders.Clone()
				req.Header.Set("X-API-Key", apiKey)
				req.Header.Set("Authorization", "Bearer "+apiKey)
				resp, err := loadClient.Do(req)
				if err == nil {
					_, _, _ = drainSSE(resp, false, false)
				}
			}()
		}
		wg2.Wait()
		time.Sleep(500 * time.Millisecond)
		mustSaveHeapProfile(adminClient, dbgBase, filepath.Join(*outDir, "post_spike_heap.pb.gz"))
		postSpikeSample = captureSample(adminClient, sutCmd.Process.Pid, dbgBase)
	}

	loadClient.CloseIdleConnections()
	time.Sleep(1 * time.Second)
	mustSaveHeapProfile(adminClient, dbgBase, filepath.Join(*outDir, "after_heap.pb.gz"))
	after := captureSample(adminClient, sutCmd.Process.Pid, dbgBase)

	close(stopMon)
	peak := <-peakCh

	// Verify real compact_accounting outbox + PG18 batch ingestion when scenario == compact_accounting.
	var compactOutboxBytes int64
	var compactBatchesIngested int64
	var compactUsageLogsFlushed int64
	if *scenario == "compact_accounting" {
		pgDB, err := sql.Open("pgx", *pgDSN)
		if err != nil {
			panic(fmt.Errorf("open pg for compact verification: %w", err))
		}
		defer pgDB.Close()

		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if st, statErr := os.Stat(outboxPath); statErr == nil {
				compactOutboxBytes = st.Size()
			}
			_ = pgDB.QueryRow(`SELECT COUNT(*) FROM compact_usage_batches`).Scan(&compactBatchesIngested)
			_ = pgDB.QueryRow(`SELECT COUNT(*) FROM usage_logs`).Scan(&compactUsageLogsFlushed)
			if compactOutboxBytes > 0 && compactBatchesIngested > 0 && compactUsageLogsFlushed >= okCnt.Load() {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	totalDone := okCnt.Load() + abortCnt.Load()
	var allocPerReq uint64
	if totalDone > 0 && mainWaveAfter.TotalAlloc > before.TotalAlloc {
		allocPerReq = (mainWaveAfter.TotalAlloc - before.TotalAlloc) / uint64(totalDone)
	}

	res := map[string]any{
		"scenario":                   *scenario,
		"sut":                        filepath.Base(*sutBin),
		"passthrough":                *passThrough,
		"verify_wire":                *verifyWire,
		"gomemlimit":                 *memLimit,
		"db_mode":                    map[bool]string{true: "postgres", false: "sqlite"}[*pgDSN != ""],
		"cache_mode":                 map[bool]string{true: "redis", false: "memory"}[*redisURL != ""],
		"cluster_compact_active":     *scenario == "compact_accounting",
		"compact_outbox_bytes":       compactOutboxBytes,
		"compact_batches_ingested":   compactBatchesIngested,
		"compact_usage_logs_flushed": compactUsageLogsFlushed,
		"thread_continue_materialized": up.threadContinueMatCnt.Load(),
		"concurrency":                *conc,
		"body_bytes":                 bodyBytes,
		"wire_bytes":                 len(reqSpecs[0].body),
		"dict_count":                 len(reqSpecs),
		"events_per_stream":          *events,
		"opened_concurrent":          openedStreams,
		"midstream_concurrent":       midStreams,
		"completed":                  okCnt.Load(),
		"aborted":                    abortCnt.Load(),
		"errors":                     errCnt.Load(),
		"first_error":                firstErr.Load(),
		"upstream_total_reqs":        up.totalReqs.Load(),
		"wire_verified_reqs":         up.wireVerifiedCnt.Load(),
		"wire_mismatch_reqs":         up.wireMismatchCnt.Load(),
		"first_wire_mismatch":        up.firstMismatch.Load(),
		"upstream_502_retries":       up.retries502.Load(),
		"upstream_preerr_retry":      up.retriesPreErr.Load(),
		"upstream_canceled":          up.canceledCnt.Load(),
		"elapsed_ms":                 elapsed.Milliseconds(),
		"alloc_bytes_per_req":        allocPerReq,
		"before":                     before,
		"open":                       openSample,
		"midstream":                  midSample,
		"post_spike":                 postSpikeSample,
		"after":                      after,
		"peak":                       peak,
	}
	raw, _ := json.MarshalIndent(res, "", "  ")
	_ = os.WriteFile(filepath.Join(*outDir, "result.json"), raw, 0o644)
	fmt.Println(string(raw))

	if errCnt.Load() > 0 || up.wireMismatchCnt.Load() > 0 {
		fmt.Fprintf(os.Stderr, "CRUCIBLE FAIL: errors=%d wire_mismatch=%d first_err=%v first_mismatch=%v\n",
			errCnt.Load(), up.wireMismatchCnt.Load(), firstErr.Load(), up.firstMismatch.Load())
		os.Exit(1)
	}
	if *scenario == "anthropic_threads" && up.threadContinueMatCnt.Load() < int64(*conc) {
		fmt.Fprintf(os.Stderr, "CRUCIBLE FAIL: anthropic_threads materialized continuations=%d < expected=%d\n",
			up.threadContinueMatCnt.Load(), *conc)
		os.Exit(1)
	}
	if *scenario == "compact_accounting" && (compactOutboxBytes == 0 || compactBatchesIngested == 0 || compactUsageLogsFlushed < okCnt.Load()) {
		fmt.Fprintf(os.Stderr, "CRUCIBLE FAIL: compact_accounting inactive/unflushed: outbox_bytes=%d batches=%d usage_logs=%d completed=%d\n",
			compactOutboxBytes, compactBatchesIngested, compactUsageLogsFlushed, okCnt.Load())
		os.Exit(1)
	}
}
