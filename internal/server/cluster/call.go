package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/looplj/axonhub/internal/contexts"
	"io"
	"net/http"
	"strings"
	"time"
)

type callerAuthorizationKey struct{}

func WithCallerAuthorization(ctx context.Context, authorization string) context.Context {
	return context.WithValue(ctx, callerAuthorizationKey{}, authorization)
}

var ownerClient = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// CallOwner forwards only an explicitly authorized management operation. The
// owner authenticates the original caller again. No inference request is replayed.
func CallOwner(ctx context.Context, c Config, channelID int, path string, input, output any) error {
	if _, ok := contexts.GetAPIKey(ctx); ok {
		path = strings.Replace(path, "/__axon/admin/", "/__axon/openapi/", 1)
	}
	owner := c.Owner(channelID)
	origin := c.Nodes[owner]
	if origin == "" {
		return fmt.Errorf("channel has no configured execution origin")
	}
	authorization, _ := ctx.Value(callerAuthorizationKey{}).(string)
	if authorization == "" {
		return fmt.Errorf("owner dispatch requires the original caller authorization")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	if projectID, ok := contexts.GetProjectID(ctx); ok {
		req.Header.Set("X-Project-ID", fmt.Sprintf("gid://axonhub/Project/%d", projectID))
	}
	res, err := ownerClient.Do(req)
	if err != nil {
		return fmt.Errorf("execution owner delivery unknown; do not replay: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("execution owner returned HTTP %d: %s", res.StatusCode, raw)
	}
	return json.Unmarshal(raw, output)
}
