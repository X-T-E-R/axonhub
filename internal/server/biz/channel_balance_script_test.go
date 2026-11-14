package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

func TestBalanceScriptExpr(t *testing.T) {
	resp := &httpclient.Response{StatusCode: 200, Headers: http.Header{"X-Credits": []string{"yes"}}, Body: []byte(`{"usage":25,"limit":100}`)}
	probe := &objects.ChannelBalanceProbe{Script: &objects.ChannelBalanceProbeScript{Mode: objects.ChannelBalanceProbeScriptModeExpr, Expression: `{remaining: body.limit-body.usage,used:body.usage,limit:body.limit,currency:"CREDITS",available:status==200 && headers["x-credits"]=="yes",resetAt:"2026-12-01T00:00:00Z"}`}}
	require.NoError(t, validateChannelBalanceProbeScript(probe.Script))
	result, err := runChannelBalanceProbeScript(context.Background(), probe, resp, 1, "test-api-key")
	require.NoError(t, err)
	require.Equal(t, 75.0, *result.Remaining)
	require.Equal(t, 25.0, *result.Used)
	require.Equal(t, 100.0, *result.Limit)
	require.True(t, *result.Available)
	require.Equal(t, "CREDITS", result.PrimaryBalance.Currency)
	require.Equal(t, 2026, result.ResetAt.Year())
	probe.Script.Expression = strings.Repeat(" ", channelKeyHealthCheckMaxPassWhenLen+1)
	require.Error(t, validateChannelBalanceProbeScript(probe.Script))
	probe.Script.Expression = `env("HOME")`
	require.Error(t, validateChannelBalanceProbeScript(probe.Script))
	probe.Script.Expression = `{currency: "USD"}`
	_, err = runChannelBalanceProbeScript(context.Background(), probe, resp, 1, "test-api-key")
	require.Error(t, err)
}

func TestBalanceScriptCommandGate(t *testing.T) {
	t.Setenv("AXONHUB_ALLOW_BALANCE_PROBE_COMMAND", "false")
	script := &objects.ChannelBalanceProbeScript{Mode: objects.ChannelBalanceProbeScriptModeCommand, Command: []string{"calc"}}
	require.ErrorContains(t, validateChannelBalanceProbeScript(script), "disabled")
	t.Setenv("AXONHUB_ALLOW_BALANCE_PROBE_COMMAND", "true")
	require.Error(t, validateChannelBalanceProbeScript(script))
	exe, err := os.Executable()
	require.NoError(t, err)
	script.Command = []string{exe, "-test.run=^TestBalanceScriptCommandHelper$", "--", "balance-json"}
	t.Setenv("AXONHUB_BALANCE_PROBE_COMMAND_ALLOWLIST", "[]")
	require.ErrorContains(t, validateChannelBalanceProbeScript(script), "AXONHUB_BALANCE_PROBE_COMMAND_ALLOWLIST")
	allowed, err := json.Marshal([][]string{script.Command})
	require.NoError(t, err)
	t.Setenv("AXONHUB_BALANCE_PROBE_COMMAND_ALLOWLIST", string(allowed))
	t.Setenv("AXONHUB_TEST_SERVICE_SECRET", "do-not-inherit")
	probe := &objects.ChannelBalanceProbe{Script: script, TimeoutMs: 1000}
	resp := &httpclient.Response{StatusCode: 200, Body: []byte(`{"balance":4}`)}
	result, err := runChannelBalanceProbeScript(context.Background(), probe, resp, 17, "child-only-key")
	require.NoError(t, err)
	require.Equal(t, 4.0, *result.Remaining)
	require.True(t, *result.Available)
	script.Command[len(script.Command)-1] = "balance-sleep"
	allowed, _ = json.Marshal([][]string{script.Command})
	t.Setenv("AXONHUB_BALANCE_PROBE_COMMAND_ALLOWLIST", string(allowed))
	probe.TimeoutMs = 20
	_, err = runChannelBalanceProbeScript(context.Background(), probe, resp, 17, "child-only-key")
	require.ErrorContains(t, err, "timed out")
}

func TestBalanceScriptCommandHelper(t *testing.T) {
	mode := os.Args[len(os.Args)-1]
	if mode == "balance-sleep" {
		time.Sleep(time.Second)
		os.Exit(0)
	}
	if mode != "balance-json" {
		return
	}
	var input map[string]any
	if json.NewDecoder(os.Stdin).Decode(&input) != nil || input["channelId"] != float64(17) || input["keyId"] != objects.ChannelAPIKeyFingerprint("child-only-key") || os.Getenv("AXONHUB_PROBE_API_KEY") != "child-only-key" || os.Getenv("AXONHUB_TEST_SERVICE_SECRET") != "" {
		os.Exit(4)
	}
	fmt.Print(`{"remaining":4,"currency":"USD","available":true}`)
	os.Exit(0)
}
