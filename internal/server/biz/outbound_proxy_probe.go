package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	outboundProxyProbeTimeout = 10 * time.Second
	outboundProxyProbeMaxBody = 16 * 1024
	outboundProxyTraceTarget  = "https://cloudflare.com/cdn-cgi/trace"
	outboundProxyIPifyTarget  = "https://api.ipify.org?format=json"
)

type outboundProxyProbeTarget struct {
	url   string
	host  string
	parse func([]byte) (string, string, error)
}

type outboundProxyProbeResult struct {
	success   bool
	exitIP    string
	country   string
	latencyMs int
	errText   string
}

var outboundProxyProbeTargets = []outboundProxyProbeTarget{
	{url: outboundProxyTraceTarget, host: "cloudflare.com", parse: parseCloudflareTraceProbeResponse},
	{url: outboundProxyIPifyTarget, host: "api.ipify.org", parse: parseIPifyProbeResponse},
}

// ProbeOutboundProxy tests the configured proxy against fixed public HTTPS
// services. It never accepts an arbitrary target URL, follows no redirects, and
// limits both total time and response-body size. Probe failures are stored as
// generic redacted reasons and returned through LastTestError, never with the
// underlying transport error that could contain proxy credentials.
func (svc *OutboundProxyService) ProbeOutboundProxy(ctx context.Context, id int) (*OutboundProxyDTO, error) {
	var err error
	ctx, err = outboundProxyWriteContext(ctx)
	if err != nil {
		return nil, err
	}

	proxy, err := svc.entFromContext(ctx).OutboundProxy.Query().Where(outboundproxy.ID(id)).Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get outbound proxy for probe: %w", err)
	}

	renderedURL, renderErr := RenderOutboundProxyURL(proxy.ProxyURL, "probe", "probe", "probe")
	var result outboundProxyProbeResult
	if renderErr != nil {
		result = outboundProxyProbeResult{errText: "configured proxy URL is invalid"}
	} else {
		result, err = svc.probeThroughProxy(ctx, renderedURL)
		if err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	var updatedID int
	err = WithOutboundProxyPolicyMutation(ctx, svc.db, func(txCtx context.Context) error {
		updated, updateErr := svc.entFromContext(txCtx).OutboundProxy.UpdateOneID(id).
			Where(outboundproxy.ProxyURLEQ(proxy.ProxyURL)).
			SetLastTestedAt(now).
			SetLastTestSuccess(result.success).
			SetLastExitIP(result.exitIP).
			SetLastCountry(result.country).
			SetLastLatencyMs(result.latencyMs).
			SetLastTestError(result.errText).
			Save(txCtx)
		if updateErr != nil {
			if ent.IsNotFound(updateErr) {
				return fmt.Errorf("outbound proxy changed while it was being probed")
			}
			return fmt.Errorf("failed to save outbound proxy probe result: %w", updateErr)
		}
		updatedID = updated.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	bumpOutboundProxyEpochAfterCommit(ctx)
	return svc.dtoByID(ctx, updatedID)
}

func (svc *OutboundProxyService) probeThroughProxy(ctx context.Context, proxyURL string) (outboundProxyProbeResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, outboundProxyProbeTimeout)
	defer cancel()

	client, err := svc.outboundProxyProbeClient(proxyURL)
	if err != nil {
		return outboundProxyProbeResult{errText: "configured proxy URL is invalid"}, nil
	}

	var last outboundProxyProbeResult
	start := time.Now()
	for _, target := range outboundProxyProbeTargets {
		if ctx.Err() != nil {
			return outboundProxyProbeResult{}, ctx.Err()
		}
		request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target.url, nil)
		if err != nil {
			// The URLs are constants; this branch protects future edits without
			// leaking target or proxy configuration details.
			last.errText = "public probe request could not be created"
			continue
		}
		request.Header.Set("Accept", "text/plain, application/json")
		request.Header.Set("User-Agent", "axonhub/1.0")

		requestStart := time.Now()
		response, err := client.Do(request)
		last.latencyMs = int(time.Since(requestStart).Milliseconds())
		if err != nil {
			last.errText = fmt.Sprintf("request to %s failed", target.host)
			if probeCtx.Err() != nil {
				break
			}
			continue
		}
		if response == nil || response.Body == nil {
			last.errText = fmt.Sprintf("response from %s was empty", target.host)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(response.Body, outboundProxyProbeMaxBody+1))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			last.errText = fmt.Sprintf("response from %s could not be read", target.host)
			continue
		}
		if len(body) > outboundProxyProbeMaxBody {
			last.errText = fmt.Sprintf("response from %s was too large", target.host)
			continue
		}
		if response.StatusCode != http.StatusOK {
			last.errText = fmt.Sprintf("%s returned HTTP %d", target.host, response.StatusCode)
			continue
		}
		exitIP, country, parseErr := target.parse(body)
		if parseErr != nil {
			last.errText = fmt.Sprintf("response from %s did not contain a public exit IP", target.host)
			continue
		}

		return outboundProxyProbeResult{
			success:   true,
			exitIP:    exitIP,
			country:   country,
			latencyMs: int(time.Since(requestStart).Milliseconds()),
		}, nil
	}
	if ctx.Err() != nil {
		return outboundProxyProbeResult{}, ctx.Err()
	}
	if last.errText == "" {
		last.errText = "public probe targets could not be reached"
	}
	if time.Since(start) > outboundProxyProbeTimeout {
		last.errText = "public probe timed out"
	}

	return last, nil
}

func (svc *OutboundProxyService) outboundProxyProbeClient(proxyURL string) (*http.Client, error) {
	var transport http.RoundTripper
	if svc.probeTransport != nil {
		transport = svc.probeTransport
	} else {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL")
		}
		native := httpclient.NewHttpClientWithProxy(&httpclient.ProxyConfig{
			Type: httpclient.ProxyTypeURL,
			URL:  proxyURL,
		}).GetNativeClient()
		if native == nil || native.Transport == nil {
			return nil, fmt.Errorf("proxy transport is not configured")
		}
		transport = native.Transport
	}

	return &http.Client{
		Transport: transport,
		Timeout:   outboundProxyProbeTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func parseCloudflareTraceProbeResponse(body []byte) (string, string, error) {
	var exitIP, country string
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			exitIP = strings.TrimSpace(value)
		case "loc":
			country = strings.ToUpper(strings.TrimSpace(value))
		}
	}
	if !isPublicProbeIP(exitIP) {
		return "", "", fmt.Errorf("probe response did not contain a public IP")
	}
	if len(country) != 2 {
		country = ""
	}

	return exitIP, country, nil
}

func parseIPifyProbeResponse(body []byte) (string, string, error) {
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("invalid probe response")
	}
	if !isPublicProbeIP(payload.IP) {
		return "", "", fmt.Errorf("probe response did not contain a public IP")
	}
	return payload.IP, "", nil
}

func isPublicProbeIP(raw string) bool {
	ip := net.ParseIP(strings.TrimSpace(raw))
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}
