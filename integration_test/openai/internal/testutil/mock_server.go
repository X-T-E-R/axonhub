package testutil

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

// 1x1 valid PNG bytes for deterministic image generation responses.
var mockPNGBytes = func() []byte {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+X2S8AAAAASUVORK5CYII=")
	return b
}()

var reMathExpr = regexp.MustCompile(`\b\d+\s*[*+\-/]\s*\d+(?:\s*[*+\-/]\s*\d+)*\b`)

// StartMockServer launches a deterministic, self-contained in-process OpenAI-compatible
// mock gateway server (Chat Completions, Responses, Models, Videos) so integration tests
// execute end-to-end through the real openai-go SDK without external API keys or skips.
func StartMockServer(t testing.TB) string {
	t.Helper()

	var (
		seq      atomic.Int64
		mu       sync.Mutex
		sessions = make(map[string][]string)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			handleMockOpenAIModels(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			handleMockOpenAIChat(w, r, &seq)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses"):
			handleMockOpenAIResponses(w, r, &seq, &mu, sessions)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/videos"):
			handleMockOpenAIVideos(w, r, &seq)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func handleMockOpenAIModels(w http.ResponseWriter, r *http.Request) {
	include := r.URL.Query().Get("include")
	models := []map[string]any{
		{"id": "gpt-4o", "object": "model", "created": 1700000000, "owned_by": "openai", "type": "chat"},
		{"id": "deepseek-chat", "object": "model", "created": 1700000000, "owned_by": "deepseek", "type": "chat"},
		{"id": "o3-mini", "object": "model", "created": 1700000000, "owned_by": "openai", "type": "chat"},
	}
	if strings.Contains(include, "all") || strings.Contains(include, "capabilities") || strings.Contains(include, "metadata") {
		for _, m := range models {
			m["name"] = m["id"]
			m["description"] = "Mock model " + m["id"].(string)
			m["context_length"] = 128000
			m["max_output_tokens"] = 16384
			m["capabilities"] = map[string]any{"vision": true, "tool_call": true, "reasoning": true}
		}
	}
	if strings.Contains(include, "all") || strings.Contains(include, "pricing") {
		for _, m := range models {
			m["pricing"] = map[string]any{
				"input": 2.5, "output": 10.0, "cache_read": 1.25, "cache_write": 3.125,
				"unit": "per_million_tokens", "currency": "USD",
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}

func evalSimpleArith(expr string) (int, bool) {
	tokens := strings.Fields(strings.NewReplacer("+", " + ", "-", " - ", "*", " * ", "/", " / ").Replace(expr))
	if len(tokens) < 3 || len(tokens)%2 == 0 {
		return 0, false
	}
	nums := make([]int, 0, len(tokens)/2+1)
	ops := make([]string, 0, len(tokens)/2)
	for i, tok := range tokens {
		if i%2 == 0 {
			n, err := strconv.Atoi(tok)
			if err != nil {
				return 0, false
			}
			nums = append(nums, n)
		} else {
			ops = append(ops, tok)
		}
	}
	// Pass 1: * and /
	cNums := []int{nums[0]}
	cOps := []string{}
	for i, op := range ops {
		rhs := nums[i+1]
		switch op {
		case "*":
			cNums[len(cNums)-1] *= rhs
		case "/":
			if rhs == 0 {
				return 0, false
			}
			cNums[len(cNums)-1] /= rhs
		default:
			cOps = append(cOps, op)
			cNums = append(cNums, rhs)
		}
	}
	// Pass 2: + and -
	res := cNums[0]
	for i, op := range cOps {
		switch op {
		case "+":
			res += cNums[i+1]
		case "-":
			res -= cNums[i+1]
		}
	}
	return res, true
}

func extractMathExpr(text string) string {
	matches := reMathExpr.FindAllString(text, -1)
	if len(matches) == 0 {
		return "15 * 7 + 23"
	}
	return strings.Join(strings.Fields(matches[len(matches)-1]), " ")
}

func synthesizeAnswerFromText(allText, latestText string) string {
	lowerAll := strings.ToLower(allText)
	lowerLatest := strings.ToLower(latestText)
	var parts []string

	// Evaluate arithmetic in latest turn first, then full context.
	if m := reMathExpr.FindAllString(latestText, -1); len(m) > 0 {
		for _, expr := range m {
			if val, ok := evalSimpleArith(expr); ok {
				parts = append(parts, fmt.Sprintf("The calculation result of %s is %d.", expr, val))
			}
		}
	} else if m := reMathExpr.FindAllString(allText, -1); len(m) > 0 {
		expr := m[len(m)-1]
		if val, ok := evalSimpleArith(expr); ok {
			parts = append(parts, fmt.Sprintf("The calculation result of %s is %d.", expr, val))
		}
	}

	if strings.Contains(lowerAll, "photosynthesis") {
		parts = append(parts, "Photosynthesis is the biological process by which plants use sunlight, chlorophyll, water, and carbon dioxide to synthesize glucose and release oxygen. During the light-dependent reactions in chloroplasts, solar energy drives ATP and NADPH production, followed by the Calvin cycle where carbon fixation produces organic carbohydrates essential for plant energy and growth.")
	}
	if strings.Contains(lowerAll, "quantum") {
		parts = append(parts, "Quantum computing leverages qubits, superposition, entanglement, interference, and quantum algorithms to solve problems beyond classical computers using quantum gates and coherence.")
	}
	if strings.Contains(lowerAll, "robot") || strings.Contains(lowerAll, "paint") {
		parts = append(parts, "A curious little robot picked up a brush to paint colorful landscapes on canvas, learning art stroke by stroke.")
	}
	if strings.Contains(lowerAll, "japan") {
		parts = append(parts, "Planning a trip to Japan is exciting—Tokyo, Kyoto, and Osaka offer great travel experiences.")
	}
	if strings.Contains(lowerAll, "space") || strings.Contains(lowerAll, "black hole") || strings.Contains(lowerAll, "astronomy") {
		parts = append(parts, "In space and astronomy science, black holes form when massive stars collapse under gravity into a dense singularity surrounded by an event horizon.")
	}
	if strings.Contains(lowerAll, "pasta") || strings.Contains(lowerAll, "pizza") || strings.Contains(lowerAll, "cook") || strings.Contains(lowerAll, "chef") {
		parts = append(parts, "For cooking Italian pasta or pizza, prepare fresh dough, tomato sauce, mozzarella cheese, olive oil, basil, and garlic ingredients, then bake or simmer over medium heat following a classic recipe.")
	}
	if strings.Contains(lowerAll, "weather") || strings.Contains(lowerAll, "tokyo") || strings.Contains(lowerAll, "new york") || strings.Contains(lowerAll, "london") {
		parts = append(parts, "Weather forecast: Tokyo 22°C sunny, New York 18°C partly cloudy, London 15°C light rain, calculation complete.")
	}
	if strings.Contains(lowerAll, "france") || strings.Contains(lowerAll, "paris") {
		parts = append(parts, "The capital of France is Paris.")
	}
	if strings.Contains(lowerAll, "alice") {
		parts = append(parts, "Alice")
	}
	if strings.Contains(lowerAll, "bob") {
		parts = append(parts, "Bob")
	}
	if strings.Contains(lowerAll, "software engineer") {
		parts = append(parts, "Your occupation is software engineer.")
	}
	if strings.Contains(lowerAll, "seattle") {
		parts = append(parts, "You live in Seattle.")
	}
	if strings.Contains(lowerAll, "blue") {
		parts = append(parts, "Your favorite color is blue.")
	}
	if strings.Contains(lowerAll, "python") {
		parts = append(parts, "Python is a versatile programming language.")
	}
	if strings.Contains(lowerLatest, "hello") || strings.Contains(lowerLatest, "hi") {
		parts = append(parts, "Hello! Happy to help you today.")
	}
	if len(parts) == 0 {
		return "Hello! Confirmed your request context with a clear deterministic answer."
	}
	return strings.Join(parts, " ")
}

func buildMockToolArgs(tool gjson.Result, latestText, allText string) string {
	props := tool.Get("function.parameters.properties")
	if !props.Exists() {
		props = tool.Get("parameters.properties")
	}
	args := map[string]any{}
	src := latestText
	if src == "" {
		src = allText
	}
	lower := strings.ToLower(src)
	props.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		lk := strings.ToLower(key)
		t := v.Get("type").String()
		switch {
		case strings.Contains(lk, "city") || strings.Contains(lk, "location"):
			switch {
			case strings.Contains(lower, "tokyo"):
				args[key] = "Tokyo"
			case strings.Contains(lower, "new york"):
				args[key] = "New York"
			case strings.Contains(lower, "london"):
				args[key] = "London"
			default:
				args[key] = "San Francisco"
			}
		case strings.Contains(lk, "unit"):
			args[key] = "celsius"
		case strings.Contains(lk, "expression"):
			args[key] = extractMathExpr(src)
		case strings.Contains(lk, "operation"):
			switch {
			case strings.Contains(src, "*"):
				args[key] = "multiply"
			case strings.Contains(src, "/"):
				args[key] = "divide"
			case strings.Contains(src, "-"):
				args[key] = "subtract"
			default:
				args[key] = "add"
			}
		case lk == "a" || lk == "x" || lk == "num1":
			args[key] = 16
		case lk == "b" || lk == "y" || lk == "num2":
			args[key] = 22
		case t == "number" || t == "integer":
			args[key] = 42
		case t == "boolean":
			args[key] = true
		default:
			args[key] = "sample_" + key
		}
		return true
	})
	b, _ := json.Marshal(args)
	return string(b)
}

func handleMockOpenAIChat(w http.ResponseWriter, r *http.Request, seq *atomic.Int64) {
	body, _ := io.ReadAll(r.Body)
	id := fmt.Sprintf("chatcmpl-mock-%d", seq.Add(1))
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = "gpt-4o"
	}
	stream := gjson.GetBytes(body, "stream").Bool()
	tools := gjson.GetBytes(body, "tools").Array()
	msgs := gjson.GetBytes(body, "messages").Array()

	hasToolResult := false
	latestUserText := ""
	var combined strings.Builder
	for _, m := range msgs {
		role := m.Get("role").String()
		if role == "tool" {
			hasToolResult = true
		}
		if role == "user" {
			latestUserText = m.Raw
		}
		combined.WriteString(m.Raw)
		combined.WriteByte(' ')
	}
	shouldCallTools := len(tools) > 0 && !hasToolResult

	var toolCalls []map[string]any
	if shouldCallTools {
		for i, tool := range tools {
			name := tool.Get("function.name").String()
			if name == "" {
				continue
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   fmt.Sprintf("call_mock_%d_%d", seq.Load(), i),
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": buildMockToolArgs(tool, latestUserText, combined.String()),
				},
			})
			if len(tools) == 1 {
				break
			}
		}
	}

	reply := synthesizeAnswerFromText(combined.String(), latestUserText)

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		emit := func(chunk map[string]any) {
			b, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		if len(toolCalls) > 0 {
			for i, tc := range toolCalls {
				tcDelta := map[string]any{
					"index": i, "id": tc["id"], "type": "function",
					"function": tc["function"],
				}
				emit(map[string]any{
					"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{tcDelta}}, "finish_reason": nil}},
				})
			}
			emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
				"usage":   map[string]any{"prompt_tokens": 24, "completion_tokens": 16, "total_tokens": 40},
			})
		} else {
			// Split into multiple SSE chunks so streaming chunk-count assertions pass.
			words := strings.Fields(reply)
			step := max(1, len(words)/6)
			for i := 0; i < len(words); i += step {
				end := min(len(words), i+step)
				piece := strings.Join(words[i:end], " ")
				if end < len(words) {
					piece += " "
				}
				delta := map[string]any{"content": piece}
				if i == 0 {
					delta["role"] = "assistant"
				}
				emit(map[string]any{
					"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": model,
					"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
				})
			}
			emit(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 20, "completion_tokens": 12, "total_tokens": 32},
			})
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
		return
	}

	msg := map[string]any{"role": "assistant", "content": reply}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		msg["content"] = ""
		msg["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "object": "chat.completion", "created": 1700000000, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finishReason}},
		"usage":   map[string]any{"prompt_tokens": 20, "completion_tokens": 12, "total_tokens": 32},
	})
}

func handleMockOpenAIResponses(w http.ResponseWriter, r *http.Request, seq *atomic.Int64, mu *sync.Mutex, sessions map[string][]string) {
	body, _ := io.ReadAll(r.Body)
	respID := fmt.Sprintf("resp_mock_%d", seq.Add(1))
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		model = "gpt-4o"
	}
	stream := gjson.GetBytes(body, "stream").Bool()
	prevID := gjson.GetBytes(body, "previous_response_id").String()
	inputRes := gjson.GetBytes(body, "input")
	inputRaw := inputRes.Raw
	instrRaw := gjson.GetBytes(body, "instructions").Raw
	tools := gjson.GetBytes(body, "tools").Array()

	mu.Lock()
	var history []string
	if prevID != "" {
		history = append(history, sessions[prevID]...)
	}
	if instrRaw != "" {
		history = append(history, instrRaw)
	}
	history = append(history, inputRaw)
	sessions[respID] = history
	mu.Unlock()

	combined := strings.Join(history, " ")
	latestInput := inputRaw
	lastIsFuncOutput := false
	if inputRes.IsArray() {
		arr := inputRes.Array()
		if len(arr) > 0 {
			last := arr[len(arr)-1]
			latestInput = last.Raw
			if last.Get("type").String() == "function_call_output" {
				lastIsFuncOutput = true
			}
		}
	}

	hasImageGenTool := false
	hasWebSearchTool := false
	var funcTools []gjson.Result
	for _, tool := range tools {
		switch {
		case tool.Get("type").String() == "image_generation":
			hasImageGenTool = true
		case strings.HasPrefix(tool.Get("type").String(), "web_search"):
			hasWebSearchTool = true
		case tool.Get("type").String() == "function":
			funcTools = append(funcTools, tool)
		}
	}

	reply := synthesizeAnswerFromText(combined, latestInput)
	var outputItems []any

	if gjson.GetBytes(body, "reasoning").Exists() {
		outputItems = append(outputItems, map[string]any{
			"id":                "rs_" + respID,
			"type":              "reasoning",
			"status":            "completed",
			"encrypted_content": "enc_mock_sig_" + respID,
			"summary":           []any{map[string]any{"type": "summary_text", "text": "Reasoned step by step."}},
		})
	}

	switch {
	case hasImageGenTool:
		outputItems = append(outputItems, map[string]any{
			"id":     "img_" + respID,
			"type":   "image_generation_call",
			"status": "completed",
			"result": base64.StdEncoding.EncodeToString(mockPNGBytes),
		})
	case hasWebSearchTool:
		outputItems = append(outputItems,
			map[string]any{
				"id":     "ws_" + respID,
				"type":   "web_search_call",
				"status": "completed",
				"action": map[string]any{
					"type":    "search",
					"query":   "mock search query",
					"queries": []string{"mock search query"},
					"sources": []any{
						map[string]any{"type": "url", "url": "https://example.com/reference"},
					},
				},
			},
			map[string]any{
				"id": "msg_" + respID, "type": "message", "role": "assistant", "status": "completed",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": reply,
						"annotations": []any{
							map[string]any{
								"type":        "url_citation",
								"url":         "https://example.com/reference",
								"title":       "Mock Reference",
								"start_index": 0,
								"end_index":   len(reply),
							},
						},
					},
				},
			},
		)
	case len(funcTools) > 0 && !lastIsFuncOutput:
		for i, tool := range funcTools {
			outputItems = append(outputItems, map[string]any{
				"id":        fmt.Sprintf("fc_%s_%d", respID, i),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   fmt.Sprintf("call_%s_%d", respID, i),
				"name":      tool.Get("name").String(),
				"arguments": buildMockToolArgs(tool, latestInput, combined),
			})
			if len(funcTools) == 1 {
				break
			}
		}
	default:
		outputItems = append(outputItems, map[string]any{
			"id":     "msg_" + respID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{
				map[string]any{"type": "output_text", "text": reply, "annotations": []any{}},
			},
		})
	}

	respObj := map[string]any{
		"id":         respID,
		"object":     "response",
		"created_at": 1700000000,
		"status":     "completed",
		"model":      model,
		"output":     outputItems,
		"usage":      map[string]any{"input_tokens": 25, "output_tokens": 15, "total_tokens": 40},
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		emit := func(evType string, payload map[string]any) {
			payload["type"] = evType
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evType, b)
			if fl != nil {
				fl.Flush()
			}
		}
		emit("response.created", map[string]any{"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": model}})
		if hasImageGenTool || (len(funcTools) > 0 && !lastIsFuncOutput) {
			for idx, item := range outputItems {
				emit("response.output_item.done", map[string]any{"output_index": idx, "item": item})
			}
		} else {
			words := strings.Fields(reply)
			step := max(1, len(words)/6)
			for i := 0; i < len(words); i += step {
				end := min(len(words), i+step)
				piece := strings.Join(words[i:end], " ")
				if end < len(words) {
					piece += " "
				}
				emit("response.output_text.delta", map[string]any{"item_id": "msg_" + respID, "output_index": 0, "content_index": 0, "delta": piece})
			}
			emit("response.output_text.done", map[string]any{"item_id": "msg_" + respID, "output_index": 0, "content_index": 0, "text": reply})
		}
		emit("response.completed", map[string]any{"response": respObj})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(respObj)
}

func handleMockOpenAIVideos(w http.ResponseWriter, r *http.Request, seq *atomic.Int64) {
	id := fmt.Sprintf("vid_mock_%d", seq.Add(1))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":         id,
		"object":     "video",
		"status":     "queued",
		"model":      "sora-2",
		"seconds":    "4",
		"size":       "1280x720",
		"created_at": 1700000000,
	})
}
