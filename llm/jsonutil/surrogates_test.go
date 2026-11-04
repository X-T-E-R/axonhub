package jsonutil

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizeSurrogates(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"high", `{"s":"\ud83d..."}`, `{"s":"\ufffd..."}`},
		{"low", `{"s":"\udef0"}`, `{"s":"\ufffd"}`},
		{"adjacent", `{"s":"\ud83d\ud83d\udef0"}`, `{"s":"\ufffd\ud83d\udef0"}`},
		{"pair", `{"s":"\uD83D\uDEF0"}`, `{"s":"\uD83D\uDEF0"}`},
		{"backslash", `{"s":"\\ud83d"}`, `{"s":"\\ud83d"}`},
		{"odd backslashes", `{"s":"\\\ud83d"}`, `{"s":"\\\ufffd"}`},
		{"escaped quote", `{"s":"\"\ud800","after":"\udfff"}`, `{"s":"\"\ufffd","after":"\ufffd"}`},
		{"boundaries", `["\ud800\udbff\udc00\udfff","\ud7ff\ue000"]`, `["\ufffd\udbff\udc00\ufffd","\ud7ff\ue000"]`},
		{"raw fidelity", " {\"z\":9007199254740993, \"\\ud800\":\"\\udef0\",\"signature\":\"a/b+==\",\"a\":1e+009} ", " {\"z\":9007199254740993, \"\\ufffd\":\"\\ufffd\",\"signature\":\"a/b+==\",\"a\":1e+009} "},
		{"literal unicode", `{"s":"🛰️"}`, `{"s":"🛰️"}`},
		{"outside strings", `\ud800`, `\ud800`},
		{"incomplete escape", `"\ud83`, `"\ud83`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(tc.input)
			got := NormalizeSurrogates(input)
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if string(input) != tc.input {
				t.Fatal("modified input")
			}
			if !bytes.Equal(NormalizeSurrogates(got), got) {
				t.Fatal("not idempotent")
			}
			if json.Valid(input) {
				var before, after any
				_ = json.Unmarshal(input, &before)
				if err := json.Unmarshal(got, &after); err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(before)
				a, _ := json.Marshal(after)
				if !bytes.Equal(a, b) {
					t.Fatalf("changed decoded value: %s != %s", a, b)
				}
			}
		})
	}
}
