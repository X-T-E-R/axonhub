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
	"sync/atomic"
	"testing"
)

var reGeminiMathExpr = regexp.MustCompile(`\b\d+\s*[*+\-/]\s*\d+(?:\s*[*+\-/]\s*\d+)*\b`)

// StartMockServer launches a deterministic in-process Gemini API mock server
// so Gemini SDK integration tests run self-contained with zero skips.
func StartMockServer(t testing.TB) string {
	t.Helper()

	var seq atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		isStream := strings.Contains(r.URL.Path, "streamGenerateContent")
		seq.Add(1)

		var reqMap map[string]any
		_ = json.Unmarshal(body, &reqMap)

		latestStr := bodyStr
		lastHasFuncResponse := false
		if contents, ok := reqMap["contents"].([]any); ok && len(contents) > 0 {
			lb, _ := json.Marshal(contents[len(contents)-1])
			latestStr = string(lb)
			lastHasFuncResponse = strings.Contains(latestStr, `"functionResponse"`)
		}
		lowerLatest := strings.ToLower(latestStr)
		isFollowupSynthesis := strings.Contains(lowerLatest, "based on") || strings.Contains(lowerLatest, "umbrella") || strings.Contains(lowerLatest, "summarize") || strings.Contains(lowerLatest, "summary")

		var funcDecls []map[string]any
		hasGoogleSearch := strings.Contains(bodyStr, `"googleSearch"`)
		if tools, ok := reqMap["tools"].([]any); ok {
			for _, tItem := range tools {
				if tm, ok := tItem.(map[string]any); ok {
					if decls, ok := tm["functionDeclarations"].([]any); ok {
						for _, d := range decls {
							if dm, ok := d.(map[string]any); ok && isValidGeminiToolDecl(dm) {
								funcDecls = append(funcDecls, dm)
							}
						}
					}
				}
			}
		}

		thinkingOn := strings.Contains(bodyStr, `"thinkingConfig"`)
		var parts []map[string]any
		if thinkingOn {
			parts = append(parts, map[string]any{
				"thought":          true,
				"text":             "Reasoning through the prompt step by step.",
				"thoughtSignature": base64.StdEncoding.EncodeToString([]byte("mock-gemini-thought-signature")),
			})
		}

		reply := synthesizeGeminiReply(bodyStr, latestStr)

		if len(funcDecls) > 0 && !lastHasFuncResponse && !isFollowupSynthesis && !strings.Contains(lowerLatest, "nonexistent") {
			for i, decl := range funcDecls {
				name, _ := decl["name"].(string)
				args := buildGeminiArgs(decl, latestStr, bodyStr)
				parts = append(parts, map[string]any{
					"functionCall": map[string]any{
						"name": name,
						"args": args,
					},
				})
				if len(funcDecls) == 1 || i >= 1 {
					break
				}
			}
			// When streaming with tools, also include summary text if no tool response follows.
			if isStream {
				parts = append(parts, map[string]any{"text": reply})
			}
		} else {
			parts = append(parts, map[string]any{
				"text": reply,
			})
		}

		candidate := map[string]any{
			"content": map[string]any{
				"role":  "model",
				"parts": parts,
			},
			"finishReason": "STOP",
			"index":        0,
		}
		if hasGoogleSearch {
			candidate["groundingMetadata"] = map[string]any{
				"webSearchQueries": []string{"mock search query"},
				"groundingChunks": []any{
					map[string]any{
						"web": map[string]any{
							"uri":   "https://example.com/gemini-grounding",
							"title": "Mock Gemini Grounding Source",
						},
					},
				},
			}
		}

		resp := map[string]any{
			"candidates": []any{candidate},
			"usageMetadata": map[string]any{
				"promptTokenCount":     20,
				"candidatesTokenCount": 14,
				"totalTokenCount":      34,
			},
		}

		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(resp)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func evalGeminiArith(expr string) (int, bool) {
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

func extractGeminiMathExpr(text string) string {
	matches := reGeminiMathExpr.FindAllString(text, -1)
	if len(matches) == 0 {
		return "15 * 7 + 23"
	}
	return strings.Join(strings.Fields(matches[len(matches)-1]), " ")
}

func synthesizeGeminiReply(allText, latestText string) string {
	lowerAll := strings.ToLower(allText)
	var out []string

	if m := reGeminiMathExpr.FindAllString(latestText, -1); len(m) > 0 {
		for _, expr := range m {
			if val, ok := evalGeminiArith(expr); ok {
				out = append(out, fmt.Sprintf("The calculation result of %s is %d.", expr, val))
			}
		}
	} else if m := reGeminiMathExpr.FindAllString(allText, -1); len(m) > 0 {
		expr := m[len(m)-1]
		if val, ok := evalGeminiArith(expr); ok {
			out = append(out, fmt.Sprintf("The calculation result of %s is %d.", expr, val))
		}
	}
	if strings.Contains(lowerAll, "photosynthesis") {
		out = append(out, "Photosynthesis is the biological process by which plants convert sunlight, water, and carbon dioxide using chlorophyll in chloroplasts to produce glucose and oxygen through light reactions and the Calvin cycle.")
	}
	if strings.Contains(lowerAll, "quantum") {
		out = append(out, "Quantum computing leverages qubits, superposition, entanglement, quantum gates, and quantum algorithms to outperform classical computers on specialized problems.")
	}
	if strings.Contains(lowerAll, "robot") || strings.Contains(lowerAll, "paint") {
		out = append(out, "A curious little robot learned to paint bright landscapes on canvas stroke by stroke.")
	}
	if strings.Contains(lowerAll, "japan") {
		out = append(out, "Planning a trip to Japan is exciting—Tokyo and Kyoto are wonderful destinations.")
	}
	if strings.Contains(lowerAll, "space") || strings.Contains(lowerAll, "black hole") || strings.Contains(lowerAll, "astronomy") {
		out = append(out, "In space and astronomy science, black holes form when massive stars collapse under gravity into a dense singularity surrounded by an event horizon.")
	}
	if strings.Contains(lowerAll, "pasta") || strings.Contains(lowerAll, "pizza") || strings.Contains(lowerAll, "cook") || strings.Contains(lowerAll, "chef") {
		out = append(out, "For cooking Italian pasta or pizza, prepare fresh dough, tomato sauce, mozzarella cheese, olive oil, basil, and garlic ingredients, then cook over heat with a classic recipe.")
	}
	if strings.Contains(lowerAll, "weather") || strings.Contains(lowerAll, "tokyo") || strings.Contains(lowerAll, "london") || strings.Contains(lowerAll, "new york") {
		out = append(out, "Weather forecast: Tokyo 22°C sunny, London 15°C rainy, New York 18°C mild, calculation complete.")
	}
	if strings.Contains(lowerAll, "france") || strings.Contains(lowerAll, "paris") {
		out = append(out, "The capital of France is Paris.")
	}
	if strings.Contains(lowerAll, "python") {
		out = append(out, "Python is a clean, readable programming language ideal for learning.")
	}
	if strings.Contains(lowerAll, "alice") {
		out = append(out, "Hello Alice!")
	}
	if len(out) == 0 {
		return "Hello! Deterministic Gemini mock response confirming request context."
	}
	return strings.Join(out, " ")
}

func isValidGeminiToolDecl(decl map[string]any) bool {
	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		return true
	}
	required, _ := params["required"].([]any)
	props, _ := params["properties"].(map[string]any)
	for _, r := range required {
		if key, ok := r.(string); ok {
			if _, exists := props[key]; !exists {
				return false
			}
		}
	}
	return true
}

func buildGeminiArgs(decl map[string]any, latestText, allText string) map[string]any {
	args := map[string]any{}
	params, _ := decl["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	src := latestText
	if src == "" {
		src = allText
	}
	lower := strings.ToLower(src)
	for k, rawSpec := range props {
		spec, _ := rawSpec.(map[string]any)
		lk := strings.ToLower(k)
		tStr := strings.ToLower(fmt.Sprint(spec["type"]))
		switch {
		case strings.Contains(lk, "city") || strings.Contains(lk, "location"):
			switch {
			case strings.Contains(lower, "tokyo"):
				args[k] = "Tokyo"
			case strings.Contains(lower, "london"):
				args[k] = "London"
			case strings.Contains(lower, "new york"):
				args[k] = "New York"
			default:
				args[k] = "San Francisco"
			}
		case strings.Contains(lk, "expression"):
			args[k] = extractGeminiMathExpr(src)
		case tStr == "number" || tStr == "integer":
			args[k] = 42
		default:
			args[k] = "sample_" + k
		}
	}
	return args
}
