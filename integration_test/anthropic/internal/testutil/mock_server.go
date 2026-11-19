package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

var reAnthropicMathExpr = regexp.MustCompile(`\b\d+\s*[*+\-/]\s*\d+(?:\s*[*+\-/]\s*\d+)*\b`)

// StartMockServer launches a deterministic in-process Anthropic Messages mock server
// so Anthropic SDK integration tests run self-contained with zero skips.
func StartMockServer(t testing.TB) string {
	t.Helper()

	var seq atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		id := fmt.Sprintf("msg_mock_%d", seq.Add(1))
		model := gjson.GetBytes(body, "model").String()
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		stream := gjson.GetBytes(body, "stream").Bool()
		thinkingEnabled := gjson.GetBytes(body, "thinking").Exists() && gjson.GetBytes(body, "thinking.type").String() != "disabled"
		tools := gjson.GetBytes(body, "tools").Array()
		msgs := gjson.GetBytes(body, "messages").Array()
		sysRaw := gjson.GetBytes(body, "system").Raw
		msgsRaw := gjson.GetBytes(body, "messages").Raw

		latestRaw := ""
		lastHasToolResult := false
		if len(msgs) > 0 {
			latestRaw = msgs[len(msgs)-1].Raw
			lastHasToolResult = strings.Contains(latestRaw, `"tool_result"`)
		}
		allText := sysRaw + " " + msgsRaw
		lowerLatest := strings.ToLower(latestRaw)
		isFollowupSynthesis := strings.Contains(lowerLatest, "based on") || strings.Contains(lowerLatest, "umbrella") || strings.Contains(lowerLatest, "summarize") || strings.Contains(lowerLatest, "summary")

		var regularTools []gjson.Result
		hasWebSearch := false
		for _, tool := range tools {
			if strings.HasPrefix(tool.Get("type").String(), "web_search") {
				hasWebSearch = true
			} else if tool.Get("name").String() != "" {
				regularTools = append(regularTools, tool)
			}
		}

		reply := synthesizeAnthropicReply(allText, latestRaw)
		var contentBlocks []map[string]any
		stopReason := "end_turn"

		if thinkingEnabled {
			contentBlocks = append(contentBlocks, map[string]any{
				"type":      "thinking",
				"thinking":  "Analyzing the question step by step.",
				"signature": "sig_mock_" + id,
			})
		}

		if len(regularTools) > 0 && !lastHasToolResult && !isFollowupSynthesis {
			stopReason = "tool_use"
			for i, tool := range regularTools {
				name := tool.Get("name").String()
				args := buildAnthropicToolInput(tool, latestRaw, allText)
				contentBlocks = append(contentBlocks, map[string]any{
					"type":  "tool_use",
					"id":    fmt.Sprintf("toolu_mock_%d_%d", seq.Load(), i),
					"name":  name,
					"input": args,
				})
				if len(regularTools) == 1 {
					break
				}
			}
		} else if hasWebSearch {
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": "Web search result: " + reply,
				"citations": []any{
					map[string]any{
						"type":            "web_search_result_location",
						"url":             "https://example.com/search-result",
						"title":           "Mock Search Citation",
						"encrypted_index": "idx_mock_1",
						"cited_text":      reply,
					},
				},
			})
		} else {
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": reply,
			})
		}

		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			emit := func(ev string, data any) {
				b, _ := json.Marshal(data)
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
				if fl != nil {
					fl.Flush()
				}
			}
			emit("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": id, "type": "message", "role": "assistant", "model": model,
					"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]any{"input_tokens": 25, "output_tokens": 1},
				},
			})
			for idx, blk := range contentBlocks {
				switch blk["type"] {
				case "thinking":
					emit("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
					emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "thinking_delta", "thinking": blk["thinking"]}})
					emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "signature_delta", "signature": blk["signature"]}})
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				case "tool_use":
					emit("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": blk["id"], "name": blk["name"], "input": map[string]any{}}})
					argJSON, _ := json.Marshal(blk["input"])
					emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(argJSON)}})
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				default:
					emit("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
					words := strings.Fields(blk["text"].(string))
					step := max(1, len(words)/6)
					for i := 0; i < len(words); i += step {
						end := min(len(words), i+step)
						piece := strings.Join(words[i:end], " ")
						if end < len(words) {
							piece += " "
						}
						emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": piece}})
					}
					if cits, ok := blk["citations"].([]any); ok {
						for _, cit := range cits {
							emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "citations_delta", "citation": cit}})
						}
					}
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				}
			}
			emit("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 18},
			})
			emit("message_stop", map[string]any{"type": "message_stop"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       contentBlocks,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 25, "output_tokens": 18},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func evalAnthropicArith(expr string) (int, bool) {
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
	res := cNums[0]
	for i, op := range cOps {
		if op == "+" {
			res += cNums[i+1]
		} else {
			res -= cNums[i+1]
		}
	}
	return res, true
}

func extractAnthropicMathExpr(text string) string {
	matches := reAnthropicMathExpr.FindAllString(text, -1)
	if len(matches) == 0 {
		return "15 * 7 + 23"
	}
	return strings.Join(strings.Fields(matches[len(matches)-1]), " ")
}

func synthesizeAnthropicReply(allText, latestText string) string {
	lowerAll := strings.ToLower(allText)
	var parts []string

	if m := reAnthropicMathExpr.FindAllString(latestText, -1); len(m) > 0 {
		for _, expr := range m {
			if val, ok := evalAnthropicArith(expr); ok {
				parts = append(parts, fmt.Sprintf("The calculation result of %s is %d.", expr, val))
			}
		}
	} else if m := reAnthropicMathExpr.FindAllString(allText, -1); len(m) > 0 {
		expr := m[len(m)-1]
		if val, ok := evalAnthropicArith(expr); ok {
			parts = append(parts, fmt.Sprintf("The calculation result of %s is %d.", expr, val))
		}
	}
	if strings.Contains(lowerAll, "photosynthesis") {
		parts = append(parts, "Photosynthesis is the biological process by which plants convert sunlight, water, and carbon dioxide using chlorophyll in chloroplasts to synthesize glucose and release oxygen through light-dependent reactions and the Calvin cycle.")
	}
	if strings.Contains(lowerAll, "quantum") {
		parts = append(parts, "Quantum computing leverages qubits, superposition, entanglement, quantum gates, and quantum algorithms to surpass classical computers on complex problems.")
	}
	if strings.Contains(lowerAll, "robot") || strings.Contains(lowerAll, "paint") {
		parts = append(parts, "A curious little robot learned to paint vibrant landscapes on canvas with careful brush strokes.")
	}
	if strings.Contains(lowerAll, "japan") {
		parts = append(parts, "Planning a trip to Japan is wonderful—Tokyo and Kyoto are top destinations.")
	}
	if strings.Contains(lowerAll, "space") || strings.Contains(lowerAll, "black hole") || strings.Contains(lowerAll, "astronomy") {
		parts = append(parts, "In space and astronomy science, black holes form when massive stars collapse under gravity into a singularity with an event horizon.")
	}
	if strings.Contains(lowerAll, "pasta") || strings.Contains(lowerAll, "pizza") || strings.Contains(lowerAll, "cook") || strings.Contains(lowerAll, "chef") {
		parts = append(parts, "For cooking Italian pasta or pizza, prepare fresh dough, tomato sauce, cheese, olive oil, basil, and garlic ingredients, then simmer or bake over heat using a classic recipe.")
	}
	if strings.Contains(lowerAll, "weather") || strings.Contains(lowerAll, "tokyo") || strings.Contains(lowerAll, "london") || strings.Contains(lowerAll, "new york") {
		parts = append(parts, "Weather forecast: Tokyo 22°C sunny, London 15°C cloudy, New York 18°C mild, calculation complete.")
	}
	if strings.Contains(lowerAll, "france") || strings.Contains(lowerAll, "paris") {
		parts = append(parts, "The capital of France is Paris.")
	}
	if strings.Contains(lowerAll, "alice") {
		parts = append(parts, "Hello Alice! Nice to meet you.")
	}
	if strings.Contains(lowerAll, "homework") || strings.Contains(lowerAll, "animals") || strings.Contains(lowerAll, "biology") || strings.Contains(lowerAll, "tutor") {
		parts = append(parts, "As your biology tutor, I am happy to help with your homework about animals.")
	}
	if strings.Contains(lowerAll, "hello") || strings.Contains(lowerAll, "hi") || strings.Contains(lowerAll, "assist") {
		parts = append(parts, "Hello! Yes, I understand your context and am happy to help.")
	}
	if len(parts) == 0 {
		return "Hello! Confirmed your context and request with a clear answer."
	}
	return strings.Join(parts, " ")
}

func buildAnthropicToolInput(tool gjson.Result, latestText, allText string) map[string]any {
	args := map[string]any{}
	src := latestText
	if src == "" {
		src = allText
	}
	lower := strings.ToLower(src)
	tool.Get("input_schema.properties").ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		lk := strings.ToLower(key)
		switch {
		case strings.Contains(lk, "city") || strings.Contains(lk, "location"):
			switch {
			case strings.Contains(lower, "tokyo"):
				args[key] = "Tokyo"
			case strings.Contains(lower, "london"):
				args[key] = "London"
			case strings.Contains(lower, "new york"):
				args[key] = "New York"
			default:
				args[key] = "San Francisco"
			}
		case strings.Contains(lk, "expression"):
			args[key] = extractAnthropicMathExpr(src)
		case v.Get("type").String() == "number" || v.Get("type").String() == "integer":
			args[key] = 42
		default:
			args[key] = "sample_" + key
		}
		return true
	})
	return args
}
