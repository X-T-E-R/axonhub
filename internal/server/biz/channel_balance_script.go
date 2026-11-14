package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func balanceScriptEnv(resp *httpclient.Response) map[string]any {
	env := map[string]any{"status": 0, "body": map[string]any{}, "headers": map[string]any{}}
	if resp != nil {
		env["status"] = resp.StatusCode
		env["body"] = decodeBalanceProbeJSON(resp.Body)
		headers := map[string]any{}
		for name, values := range resp.Headers {
			headers[strings.ToLower(name)] = strings.Join(values, ", ")
		}
		env["headers"] = headers
	}
	return env
}

func compileBalanceScript(expression string) (*vm.Program, error) {
	if strings.TrimSpace(expression) == "" || len(expression) > channelKeyHealthCheckMaxPassWhenLen {
		return nil, errors.New("balance expression is empty or too long")
	}
	p, err := expr.Compile(expression, expr.Env(balanceScriptEnv(nil)), expr.AsKind(reflect.Map), expr.DisableAllBuiltins(), expr.MaxNodes(channelKeyHealthCheckMaxPassWhenNode))
	if err != nil {
		return nil, errors.New("invalid balance expression: expected a map")
	}
	return p, nil
}

// The local operator explicitly admits exact argv arrays in this JSON allowlist.
// No shell, PATH lookup, caller-selected arguments or inherited service secrets.
func validateBalanceCommand(argv []string) error {
	if os.Getenv("AXONHUB_ALLOW_BALANCE_PROBE_COMMAND") != "true" {
		return errors.New("balance command mode is disabled")
	}
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || len(argv) > 32 {
		return errors.New("balance command requires a locally approved absolute executable and argument list")
	}
	var allowed [][]string
	if json.Unmarshal([]byte(os.Getenv("AXONHUB_BALANCE_PROBE_COMMAND_ALLOWLIST")), &allowed) == nil {
		for _, candidate := range allowed {
			if slices.Equal(candidate, argv) {
				return nil
			}
		}
	}
	return errors.New("balance command is not in AXONHUB_BALANCE_PROBE_COMMAND_ALLOWLIST")
}

func validateChannelBalanceProbeScript(script *objects.ChannelBalanceProbeScript) error {
	if script == nil {
		return nil
	}
	switch script.Mode {
	case "", objects.ChannelBalanceProbeScriptModeNone:
		return nil
	case objects.ChannelBalanceProbeScriptModeExpr:
		_, err := compileBalanceScript(script.Expression)
		return err
	case objects.ChannelBalanceProbeScriptModeCommand:
		return validateBalanceCommand(script.Command)
	default:
		return errors.New("unsupported balance probe script mode")
	}
}

type boundedBalanceOutput struct{ bytes.Buffer }

func (b *boundedBalanceOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<10 {
		return 0, errors.New("balance command output too large")
	}
	return b.Buffer.Write(p)
}

func runChannelBalanceProbeScript(ctx context.Context, probe *objects.ChannelBalanceProbe, resp *httpclient.Response, channelID int, key string) (objects.ChannelKeyBalanceSnapshot, error) {
	var result objects.ChannelKeyBalanceSnapshot
	env := balanceScriptEnv(resp)
	var values map[string]any
	switch probe.Script.Mode {
	case objects.ChannelBalanceProbeScriptModeExpr:
		program, err := compileBalanceScript(probe.Script.Expression)
		if err != nil {
			return result, err
		}
		value, err := expr.Run(program, env)
		if err != nil {
			return result, errors.New("balance expression evaluation failed")
		}
		values, _ = value.(map[string]any)
	case objects.ChannelBalanceProbeScriptModeCommand:
		argv := probe.Script.Command
		if err := validateBalanceCommand(argv); err != nil {
			return result, err
		}
		env["channelId"], env["keyId"] = channelID, objects.ChannelAPIKeyFingerprint(key)
		input, err := json.Marshal(env)
		if err != nil {
			return result, errors.New("could not encode balance command input")
		}
		timeout := probe.TimeoutMs
		if timeout <= 0 {
			timeout = channelKeyHealthCheckHTTPDefaultMs
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(min(timeout, maxChannelKeyHealthCheckHTTPTimeoutMs))*time.Millisecond)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = []string{"AXONHUB_PROBE_API_KEY=" + key}
		cmd.Stdin = bytes.NewReader(input)
		cmd.WaitDelay = time.Second
		var output boundedBalanceOutput
		cmd.Stdout = &output
		if err := cmd.Run(); err != nil {
			return result, errors.New("balance command failed or timed out")
		}
		if json.Unmarshal(output.Bytes(), &values) != nil {
			return result, errors.New("invalid balance command JSON output")
		}
	}
	if values == nil {
		return result, errors.New("balance script must return a map")
	}
	result = baseBalanceSnapshot(resp.StatusCode)
	currency := strings.ToUpper(stringAt(values, "currency"))
	if !isValidBalanceCurrencyCode(currency) || currency == strings.ToUpper(key) {
		currency = ""
	}
	label := stringAt(values, "label")
	if len(label) > 100 || (key != "" && strings.Contains(label, key)) {
		label = ""
	}
	result.Available = boolFromAny(valueAt(values, "available"))
	if reset, err := time.Parse(time.RFC3339, stringAt(values, "resetAt")); err == nil {
		result.ResetAt = &reset
	}
	for _, f := range []struct {
		key   string
		kind  objects.BalanceAmountKind
		field **float64
	}{
		{"remaining", objects.BalanceAmountKindRemaining, &result.Remaining},
		{"used", objects.BalanceAmountKindUsed, &result.Used},
		{"limit", objects.BalanceAmountKindLimit, &result.Limit},
		{"total", objects.BalanceAmountKindTotal, nil},
	} {
		amount, ok := amountFromAny(valueAt(values, f.key))
		if !ok || amount != amount || amount > 1e100 || amount < -1e100 {
			continue
		}
		if f.field != nil {
			*f.field = &amount
		}
		result.Components = append(result.Components, objects.BalanceAmount{Amount: amount, Currency: currency, Kind: f.kind, Label: label, ExpiresAt: result.ResetAt})
		if f.kind == objects.BalanceAmountKindRemaining {
			result.PrimaryBalance = &result.Components[len(result.Components)-1]
		}
	}
	if result.Limit == nil {
		if total, ok := amountFromAny(valueAt(values, "total")); ok && total > 0 && total <= 1e100 {
			result.Limit = &total
		}
	}
	if len(result.Components) == 0 && result.Available == nil {
		return result, errors.New("balance script returned no quota fields")
	}
	return result, nil
}
