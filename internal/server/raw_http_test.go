package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/stretchr/testify/require"
)

func rawAdmin(t *testing.T, f *nativeDecisionFixture, query string, variables map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	require.NoError(t, err)
	req, err := http.NewRequest("POST", f.server.URL+"/admin/graphql", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.adminToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode, string(data))
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func createRawChannel(t *testing.T, f *nativeDecisionFixture, baseURL, name, authType, header string) int {
	t.Helper()
	out := rawAdmin(t, f, `mutation($input:CreateChannelInput!){createChannel(input:$input){id settings{rawHTTP{enabled resource authType authHeader}}}}`, map[string]any{"input": map[string]any{
		"type": "openai", "name": name, "baseURL": baseURL, "supportedModels": []string{name}, "defaultTestModel": name, "autoSyncSupportedModels": false,
		"credentials": map[string]any{"apiKeys": []string{"synthetic-upstream-" + name}},
		"settings":    map[string]any{"rawHTTP": map[string]any{"enabled": true, "resource": name, "authType": authType, "authHeader": header}, "proxy": map[string]any{"type": "disabled"}},
	}})
	require.Nil(t, out["errors"], fmt.Sprint(out))
	saved := out["data"].(map[string]any)["createChannel"].(map[string]any)
	require.Equal(t, name, saved["settings"].(map[string]any)["rawHTTP"].(map[string]any)["resource"])
	id, err := strconv.Atoi(saved["id"].(string)[strings.LastIndex(saved["id"].(string), "/")+1:])
	require.NoError(t, err)
	enabled := rawAdmin(t, f, `mutation($id:ID!){updateChannelStatus(id:$id,status:enabled){status}}`, map[string]any{"id": saved["id"]})
	require.Nil(t, enabled["errors"], fmt.Sprint(enabled))
	return id
}

type rawCapture struct {
	body      []byte
	header    http.Header
	uri, host string
}

func TestRawHTTPBytesHeadersStatusesAndManagement(t *testing.T) {
	f := newNativeDecisionFixture(t)
	received := make(chan rawCapture, 32)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		received <- rawCapture{body: body, header: r.Header.Clone(), uri: r.RequestURI, host: r.Host}
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		w.Header().Set("X-Provider-Future", "opaque")
		w.Header().Set("Connection", "X-Response-Hop")
		w.Header().Set("X-Response-Hop", "remove")
		w.Header().Set("Retry-After", "7")
		if r.Header.Get("Content-Encoding") != "" {
			w.Header().Set("Content-Encoding", r.Header.Get("Content-Encoding"))
		}
		status := 200
		if s := r.URL.Query().Get("status"); s != "" {
			status, _ = strconv.Atoi(s)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	ids := []int{
		createRawChannel(t, f, upstream.URL+"/prefix%2Ffixed", "exa-search", "header", "x-api-key"),
		createRawChannel(t, f, upstream.URL, "tavily-search", "bearer", ""),
		createRawChannel(t, f, upstream.URL+"/v1", "openai-images", "bearer", ""),
	}
	var multipartBody bytes.Buffer
	mw := multipart.NewWriter(&multipartBody)
	part, err := mw.CreateFormFile("image", "sample.png")
	require.NoError(t, err)
	_, _ = part.Write([]byte{0, 255, 1, 13, 10})
	require.NoError(t, mw.WriteField("prompt", "edit 原样"))
	require.NoError(t, mw.Close())
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte("opaque compressed\x00payload"))
	require.NoError(t, zw.Close())
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		index                       int
		path, contentType, encoding string
		body                        []byte
		status                      int
	}{
		{0, "/search?q=%2F%26&a=1&a=2&flag&status=200", "application/json", "", []byte("{\n \"query\":\"x\",\"future\":9007199254740993,\"url\":\"https://evil.invalid/\" }"), 200},
		{1, "/search?status=422", "text/plain; charset=utf-8", "", []byte("not JSON, no model\x00"), 422},
		{2, "/images/edits?status=429", mw.FormDataContentType(), "", multipartBody.Bytes(), 429},
		{2, "/images/generations?status=503", "application/octet-stream", "", []byte{0, 1, 255, 13, 10}, 503},
		{0, "/a%2Fb/%E5%9B%BE?x=%FF&x=+&status=400", "application/x-custom", "gzip", compressed.Bytes(), 400},
	} {
		req, err := http.NewRequest("POST", fmt.Sprintf("%s/raw/%d%s", f.server.URL, ids[tc.index], tc.path), bytes.NewReader(tc.body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+f.key.Key)
		req.Header.Set("X-Api-Key", f.key.Key)
		req.Header.Set("Api-Key", f.key.Key)
		req.Header.Set("X-Goog-Api-Key", f.key.Key)
		req.Header.Set("X-Google-Api-Key", f.key.Key)
		req.Header.Set("Content-Type", tc.contentType)
		req.Header.Set("Accept", "application/custom")
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Connection", "X-Client-Hop")
		req.Header.Set("X-Client-Hop", "drop")
		req.Header.Set("X-Project-ID", "9999")
		req.Header.Set("Cookie", "session=synthetic")
		req.Header.Set("Content-Encoding", tc.encoding)
		res, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equal(t, tc.status, res.StatusCode)
		require.Equal(t, tc.body, body)
		require.Equal(t, tc.contentType, res.Header.Get("Content-Type"))
		require.Equal(t, tc.encoding, res.Header.Get("Content-Encoding"))
		require.Equal(t, "7", res.Header.Get("Retry-After"))
		require.Equal(t, "opaque", res.Header.Get("X-Provider-Future"))
		require.Empty(t, res.Header.Get("X-Response-Hop"))
		capture := <-received
		require.Equal(t, tc.body, capture.body)
		require.Equal(t, tc.contentType, capture.header.Get("Content-Type"))
		require.Equal(t, "application/custom", capture.header.Get("Accept"))
		require.Equal(t, "gzip", capture.header.Get("Accept-Encoding"))
		require.NotContains(t, fmt.Sprint(capture.header), f.key.Key)
		for _, name := range []string{"Api-Key", "X-Goog-Api-Key", "X-Google-Api-Key", "X-Client-Hop", "X-Project-ID", "Cookie", "Connection"} {
			if name != "Connection" {
				require.Empty(t, capture.header.Get(name), name)
			}
		}
		prefix := []string{"/prefix%2Ffixed", "", "/v1"}[tc.index]
		require.Equal(t, prefix+tc.path, capture.uri)
		require.Equal(t, strings.TrimPrefix(upstream.URL, "http://"), capture.host)
		if tc.index == 0 {
			require.Equal(t, "synthetic-upstream-exa-search", capture.header.Get("X-Api-Key"))
			require.Empty(t, capture.header.Get("Authorization"))
		} else {
			require.Equal(t, "Bearer synthetic-upstream-"+[]string{"exa-search", "tavily-search", "openai-images"}[tc.index], capture.header.Get("Authorization"))
			require.Empty(t, capture.header.Get("X-Api-Key"))
		}
	}
	require.EqualValues(t, 5, calls.Load(), "4xx/429/5xx are never retried")
	rows := f.db.Request.Query().Where(request.FormatEQ("http/raw")).AllX(f.ctx)
	require.Len(t, rows, 5)
	for _, row := range rows {
		require.Equal(t, f.key.ID, row.APIKeyID)
		require.Equal(t, "null", string(row.RequestBody))
		require.Empty(t, row.RequestHeaders)
		require.Empty(t, row.ResponseBody)
		require.Equal(t, "omitted", row.EvidenceDisposition.RequestBody.Outcome)
	}
	require.Zero(t, f.db.UsageLog.Query().CountX(f.ctx))
	// Real JWT update/readback, including a partial address edit validated against
	// the stored raw configuration, not merely against fields in this mutation.
	gid := fmt.Sprintf("gid://axonhub/Channel/%d", ids[0])
	out := rawAdmin(t, f, `mutation($id:ID!,$input:UpdateChannelInput!){updateChannel(id:$id,input:$input){name baseURL defaultEndpoints{apiFormat} settings{rawHTTP{enabled resource authType authHeader}}}}`, map[string]any{"id": gid, "input": map[string]any{"name": "exa-edited", "baseURL": upstream.URL + "/edited"}})
	require.Nil(t, out["errors"], fmt.Sprint(out))
	updated := out["data"].(map[string]any)["updateChannel"].(map[string]any)
	require.Empty(t, updated["defaultEndpoints"], "raw storage carrier must not advertise LLM endpoints")
	require.Equal(t, "exa-search", updated["settings"].(map[string]any)["rawHTTP"].(map[string]any)["resource"])
	out = rawAdmin(t, f, `mutation($id:ID!,$input:UpdateChannelInput!){updateChannel(id:$id,input:$input){id}}`, map[string]any{"id": gid, "input": map[string]any{"baseURL": "https://user:password@evil.invalid/?redirect=true"}})
	require.NotNil(t, out["errors"])
	_, err = f.channelService.SyncChannelModels(f.ctx, ids[0], nil)
	require.ErrorContains(t, err, "resource alias")
	// The manual tester and the scheduled built-in key tester must also stop
	// before sending a chat request to an opaque API.
	channelID := objects.GUID{ID: ids[0]}
	testResult, err := f.tester.TestChannel(f.ctx, channelID, nil, nil)
	require.NoError(t, err)
	require.False(t, testResult.Success)
	healthResult := f.tester.TestSingleChannelAPIKey(f.ctx, channelID, "synthetic-upstream-exa-search", nil, nil)
	require.False(t, healthResult.Success)
	// A raw alias cannot enter the old LLM pipeline even with a model JSON body.
	require.NoError(t, f.channelService.ReloadEnabledChannelsCache(f.ctx))
	status, _, _ := f.post(t, "/v1/chat/completions", `{"model":"exa-search","messages":[{"role":"user","content":"x"}]}`, f.key.Key, nil)
	require.NotEqual(t, 200, status)
	require.EqualValues(t, 5, calls.Load())
	status, _, _ = f.post(t, "/v1/systemone", nativeSystemBody("direct", ""), f.key.Key, nil)
	require.Equal(t, 200, status)
	require.EqualValues(t, 1, f.calls.Load())
}

func TestRawHTTPAuthorizationIsolationAndRevocation(t *testing.T) {
	f := newNativeDecisionFixture(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("allowed"))
	}))
	defer upstream.Close()
	id := createRawChannel(t, f, upstream.URL, "raw-resource", "bearer", "")
	owner := f.db.User.Create().SetEmail("raw-portal@example.test").SetPassword("synthetic").SetStatus(user.StatusActivated).SetAccessMode(user.AccessModePortal).SaveX(f.ctx)
	models := []string{"raw-resource"}
	profiles := &objects.ProjectProfiles{ActiveProfile: "raw", Profiles: []objects.ProjectProfile{{Name: "raw", ModelIDs: &models, ChannelIDs: []int{id}}}}
	group := f.db.Project.Create().SetName("raw-group").SetProfiles(profiles).SaveX(f.ctx)
	f.db.UserProject.Create().SetUserID(owner.ID).SetProjectID(group.ID).SaveX(f.ctx)
	key := f.db.APIKey.Create().SetName("raw-portal").SetKey("ah-raw-synthetic").SetProjectID(group.ID).SetUserID(owner.ID).SetPortalManaged(true).SetType(apikey.TypePersonal).SetScopes([]string{"write_requests"}).SaveX(f.ctx)
	require.NoError(t, f.channelService.ReloadEnabledChannelsCache(f.ctx))
	modelService := biz.NewModelService(biz.ModelServiceParams{ChannelService: f.channelService, SystemService: f.system, Ent: f.db})
	portal := biz.NewPortalService(biz.PortalServiceParams{Ent: f.db, Keys: f.keyService, Models: modelService, System: f.system})
	catalog, err := portal.Models(contexts.WithUser(f.ctx, owner), group.ID, nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	require.Equal(t, "raw-resource", catalog[0].ID)
	require.Equal(t, []string{"http/raw"}, catalog[0].APIFormats)
	path := fmt.Sprintf("/raw/%d/search", id)
	status, _, _ := f.post(t, path, "no model", key.Key, nil)
	require.Equal(t, 200, status)
	require.EqualValues(t, 1, calls.Load())
	for _, k := range []*objects.APIKeyProfiles{
		{ActiveProfile: "narrow", Profiles: []objects.APIKeyProfile{{Name: "narrow", ModelIDs: []string{"other"}}}},
		{ActiveProfile: "narrow", Profiles: []objects.APIKeyProfile{{Name: "narrow", ChannelIDs: []int{f.channels[0].ID}}}},
	} {
		f.db.APIKey.UpdateOne(key).SetProfiles(k).SaveX(f.ctx)
		status, _, _ = f.post(t, path, "opaque", key.Key, nil)
		require.Equal(t, 403, status)
	}
	f.db.APIKey.UpdateOne(key).SetProfiles(&objects.APIKeyProfiles{}).SaveX(f.ctx)
	models = []string{"other"}
	f.db.Project.UpdateOne(group).SetProfiles(profiles).SaveX(f.ctx)
	status, _, _ = f.post(t, path, "opaque", key.Key, nil)
	require.Equal(t, 403, status)
	models = []string{"raw-resource"}
	profiles.Profiles[0].ChannelIDs = []int{f.channels[0].ID}
	f.db.Project.UpdateOne(group).SetProfiles(profiles).SaveX(f.ctx)
	status, _, _ = f.post(t, path, "opaque", key.Key, nil)
	require.Equal(t, 403, status)
	profiles.Profiles[0].ChannelIDs = []int{id}
	f.db.Project.UpdateOne(group).SetProfiles(profiles).SaveX(f.ctx)
	f.db.APIKey.UpdateOne(key).SetScopes([]string{"read_channels"}).SaveX(f.ctx)
	status, _, _ = f.post(t, path, "opaque", key.Key, nil)
	require.Equal(t, 403, status)
	f.db.APIKey.UpdateOne(key).SetScopes([]string{"write_requests"}).SaveX(f.ctx)
	f.db.Channel.UpdateOneID(id).SetStatus(channel.StatusDisabled).SaveX(f.ctx)
	status, _, _ = f.post(t, path, "opaque", key.Key, nil)
	require.Equal(t, 403, status)
	require.EqualValues(t, 1, calls.Load())
}

func TestRawHTTPStreamCancellationAndFixedDestination(t *testing.T) {
	f := newNativeDecisionFixture(t)
	canceled := make(chan struct{}, 1)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			fmt.Fprint(w, "event: future\r\ndata: {\"future\":9007199254740993}\r\n\r\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		if r.URL.Path == "/truncate" {
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, "opaque-prefix")
			return
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "https://evil.invalid/stolen")
			w.WriteHeader(307)
			return
		}
		require.Equal(t, "//evil.invalid/path", r.URL.Path)
		fmt.Fprint(w, "fixed-origin")
	}))
	defer upstream.Close()
	id := createRawChannel(t, f, upstream.URL, "raw-stream", "bearer", "")
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/raw/%d/stream", f.server.URL, id), strings.NewReader("opaque"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.key.Key)
	res, err := f.server.Client().Do(req)
	require.NoError(t, err)
	expected := []byte("event: future\r\ndata: {\"future\":9007199254740993}\r\n\r\n")
	data := make([]byte, len(expected))
	_, err = io.ReadFull(res.Body, data)
	require.NoError(t, err)
	require.Equal(t, expected, data)
	cancel()
	require.NoError(t, res.Body.Close())
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream not canceled")
	}
	require.Eventually(t, func() bool {
		return f.db.Request.Query().Where(request.FormatEQ("http/raw"), request.StatusEQ(request.StatusCanceled)).CountX(f.ctx) == 1
	}, 2*time.Second, 10*time.Millisecond)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, suffix := range []string{"/redirect", "//evil.invalid/path"} {
		req, err := http.NewRequest("POST", fmt.Sprintf("%s/raw/%d%s", f.server.URL, id, suffix), nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+f.key.Key)
		res, err := client.Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		res.Body.Close()
		if suffix == "/redirect" {
			require.Equal(t, 307, res.StatusCode)
			require.Equal(t, "https://evil.invalid/stolen", res.Header.Get("Location"))
		} else {
			require.Equal(t, "fixed-origin", string(data))
		}
	}
	for _, suffix := range []string{"/%2e%2e/stolen", "/a/%2e/stolen", "/%5cstolen"} {
		status, _, _ := f.post(t, fmt.Sprintf("/raw/%d%s", id, suffix), "opaque", f.key.Key, nil)
		require.Equal(t, 400, status)
	}
	req, err = http.NewRequest("POST", fmt.Sprintf("%s/raw/%d/truncate", f.server.URL, id), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.key.Key)
	res, err = client.Do(req)
	require.NoError(t, err)
	data, err = io.ReadAll(res.Body)
	require.Error(t, err)
	require.Equal(t, "opaque-prefix", string(data), "stream interruption must not append an LLM error body")
	res.Body.Close()
	require.EqualValues(t, 4, calls.Load())
}
