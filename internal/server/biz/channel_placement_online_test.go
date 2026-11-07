package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestOnlineChannelPlacement(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:placement-"+uuid.NewString()+"?mode=memory&_fk=0")
	defer client.Close()
	ctx := contexts.WithUser(authz.WithTestBypass(context.Background()), &ent.User{IsOwner: true})
	ctx = cluster.WithCallerAuthorization(ctx, "Bearer synthetic-admin")

	var upstreamCalls atomic.Int32
	accepted, finish := make(chan struct{}), make(chan struct{})
	var release sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) == 1 {
			close(accepted)
			<-finish
		}
		_, _ = fmt.Fprint(w, "finished")
	}))
	defer upstream.Close()
	defer release.Do(func() { close(finish) })
	create := func(name, key, model string, status channel.Status) *ent.Channel {
		return client.Channel.Create().SetName(name).SetType(channel.TypeOpenai).SetStatus(status).SetBaseURL(upstream.URL).
			SetCredentials(objects.ChannelCredentials{APIKey: key}).SetSupportedModels([]string{model}).SetDefaultTestModel(model).
			SetTags([]string{name}).SaveX(ctx)
	}
	moving := create("moving", "synthetic-shared", "movable", channel.StatusEnabled)
	archived := create("archived", "synthetic-shared", "archived", channel.StatusArchived)
	rotated := create("rotated", "synthetic-rotated", "disabled", channel.StatusDisabled)
	controlChannel := create("other-control", "synthetic-control", "control", channel.StatusEnabled)
	sourceChannel := create("other-source", "synthetic-source", "source", channel.StatusEnabled)
	targetChannel := create("other-target", "synthetic-target", "target", channel.StatusEnabled)
	thirdChannel := create("other-third", "synthetic-third", "third", channel.StatusEnabled)
	group := client.Project.Create().SetName("placement fixture").SaveX(ctx)
	key := client.APIKey.Create().SetName("placement fixture").SetKey("ah-synthetic-placement").SetType(apikey.TypeUser).
		SetProjectID(group.ID).SetScopes([]string{string(scopes.ScopeReadChannels)}).SaveX(ctx)

	var target *ChannelService
	var rejectPrepare, loseReadback atomic.Bool
	var prepares atomic.Int32
	targetRPC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer synthetic-admin" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var value any
		var err error
		switch req.URL.Path {
		case "/__axon/admin/placement/prepare":
			prepares.Add(1)
			if rejectPrepare.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var plan PlacementPlan
			err = json.NewDecoder(req.Body).Decode(&plan)
			if err == nil {
				value, err = target.PreparePlacement(ctx, plan)
			}
		case "/__axon/admin/placement/readback":
			if loseReadback.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var input PlacementReadbackInput
			err = json.NewDecoder(req.Body).Decode(&input)
			if err == nil {
				value, err = target.ReadPlacementOnNode(ctx, input)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer targetRPC.Close()

	// There is deliberately no source HTTP origin to contact. Its existing
	// outbound still finishes, and it observes the saved owner on reload.
	nodes := map[string]string{"control": "http://127.0.0.1:1", "source": "http://127.0.0.1:2", "target": targetRPC.URL, "third": "http://127.0.0.1:3"}
	bindings := map[int]cluster.ChannelBinding{
		moving.ID: {Owner: "source", Identity: "account"}, archived.ID: {Owner: "source", Identity: "alternate-label"},
		rotated.ID: {Owner: "source", Identity: "account"}, controlChannel.ID: {Owner: "control", Identity: "control"},
		targetChannel.ID: {Owner: "target", Identity: "target"}, thirdChannel.ID: {Owner: "third", Identity: "third"},
		sourceChannel.ID: {Owner: "source", Identity: "source"},
	}
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := NewSystemService(SystemServiceParams{Ent: client, CacheConfig: cache})
	makeNode := func(id string) *ChannelService {
		svc := NewChannelService(ChannelServiceParams{Ent: client, CacheConfig: cache, SystemService: system, HttpClient: httpclient.NewHttpClient(),
			Node: cluster.Config{Enabled: true, NodeID: id, ControlNode: "control", Nodes: nodes, Channels: bindings}})
		t.Cleanup(svc.Stop)
		return svc
	}
	source, control, third := makeNode("source"), makeNode("control"), makeNode("third")
	target = makeNode("target")
	models := NewModelService(ModelServiceParams{Ent: client, SystemService: system, ChannelService: control})
	initial, err := models.BuildExecutionManifest(ctx)
	require.NoError(t, err)
	require.Zero(t, initial.PlacementVersion)
	unaffected := map[*ChannelService]*Channel{
		control: control.GetEnabledChannel(controlChannel.ID), source: source.GetEnabledChannel(sourceChannel.ID),
		target: target.GetEnabledChannel(targetChannel.ID), third: third.GetEnabledChannel(thirdChannel.ID),
	}

	plan, err := control.PreviewPlacement(ctx, moving.ID, "target")
	require.NoError(t, err)
	require.Equal(t, "source", plan.From)
	ids := lo.Map(plan.Channels, func(peer PlacementChannel, _ int) int { return peer.ID })
	require.Equal(t, []int{moving.ID, archived.ID, rotated.ID}, ids)
	input := PlacementSaveInput{ChannelID: moving.ID, Target: "target", ExpectedVersion: 0, RequestID: "move-1", ChannelIDs: ids}
	reader := contexts.WithUser(context.Background(), &ent.User{Scopes: []string{string(scopes.ScopeReadChannels)}})
	reader, err = authz.WithPrincipal(reader, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)
	_, err = control.SavePlacement(reader, input)
	require.ErrorContains(t, err, "write_channels")
	bad := input
	bad.ChannelIDs = []int{moving.ID}
	_, err = control.SavePlacement(ctx, bad)
	require.ErrorContains(t, err, "affected channels changed")

	rejectPrepare.Store(true)
	_, err = control.SavePlacement(ctx, input)
	require.ErrorContains(t, err, "current owner unchanged")
	require.True(t, source.OwnsChannel(moving.ID))
	require.False(t, target.OwnsChannel(moving.ID))
	rejectPrepare.Store(false)

	// Admit the original request and keep its upstream connection open through
	// the placement edit. No gate is paused and no transport call is replayed.
	allowed, err := source.AuthorizeExecutionChannel(ctx, moving.ID, key)
	require.NoError(t, err)
	require.True(t, allowed)
	old := source.GetEnabledChannel(moving.ID)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("request panic: %v", p)
			}
		}()
		req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
		if err != nil {
			done <- err
			return
		}
		response, err := old.HTTPClient.GetNativeClient().Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("original request did not reach upstream")
	}
	loseReadback.Store(true)
	out, err := control.SavePlacement(ctx, input)
	require.NoError(t, err)
	require.Equal(t, "target", out.Owner)
	require.EqualValues(t, 1, out.Version)
	require.Equal(t, "target_unavailable", out.TargetError)
	require.Nil(t, out.Target)
	require.Nil(t, out.WorkerAck)
	require.EqualValues(t, 1, upstreamCalls.Load())
	select {
	case err := <-done:
		t.Fatalf("original request ended before release: %v", err)
	default:
	}

	for _, node := range []*ChannelService{control, source, target, third} {
		require.NoError(t, node.enabledChannelsCache.Load(ctx, false))
	}
	require.Equal(t, moving.UpdatedAt, client.Channel.GetX(ctx, moving.ID).UpdatedAt, "placement does not edit channel metadata")
	require.Equal(t, "target", source.GetEnabledChannel(moving.ID).RemoteOwner)
	require.Empty(t, target.GetEnabledChannel(moving.ID).RemoteOwner)
	allowed, err = source.AuthorizeExecutionChannel(ctx, moving.ID, key)
	require.NoError(t, err)
	require.False(t, allowed)
	allowed, err = target.AuthorizeExecutionChannel(ctx, moving.ID, key)
	require.NoError(t, err)
	require.True(t, allowed)
	for _, peer := range []struct {
		svc *ChannelService
		ch  *ent.Channel
	}{{control, controlChannel}, {source, sourceChannel}, {target, targetChannel}, {third, thirdChannel}} {
		allowed, err := peer.svc.AuthorizeExecutionChannel(ctx, peer.ch.ID, key)
		require.NoError(t, err)
		require.True(t, allowed)
		require.Same(t, unaffected[peer.svc], peer.svc.GetEnabledChannel(peer.ch.ID), "unrelated provider must not restart")
	}
	release.Do(func() { close(finish) })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("old request did not finish")
	}
	require.EqualValues(t, 1, upstreamCalls.Load())

	loseReadback.Store(false)
	out, err = control.SavePlacement(ctx, input)
	require.NoError(t, err)
	require.EqualValues(t, 2, prepares.Load(), "idempotent response must not prepare again")
	require.EqualValues(t, 1, out.Version)
	require.True(t, out.Target.Ready)
	require.Equal(t, channel.StatusArchived, client.Channel.GetX(ctx, archived.ID).Status)
	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, rotated.ID).Status)
	restarted := makeNode("source")
	require.Equal(t, "target", restarted.NodeConfig().Owner(archived.ID))
	require.Equal(t, "target", restarted.NodeConfig().Owner(rotated.ID))
	filter, err := control.QueryChannels(ctx, QueryChannelsInput{ExecutionMachine: lo.ToPtr("target"), First: lo.ToPtr(20)})
	require.NoError(t, err)
	require.Equal(t, 4, filter.TotalCount)

	manifest, err := models.BuildExecutionManifest(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, manifest.PlacementVersion)
	route, err := models.ExecutionRouteForKey(ctx, key, "movable")
	require.NoError(t, err)
	require.Equal(t, "target", route.Owner)
	require.Contains(t, manifest.Models, ModelExecutionRoute{Model: "movable", Owner: "target", Owners: []string{"target"}, Status: "ready"})
	require.Error(t, control.AcknowledgePlacementWorker(ctx, PlacementWorkerAck{Version: 2, WorkerRevision: "future"}))
	ack := PlacementWorkerAck{Version: 1, WorkerRevision: "fixture-deployment"}
	require.NoError(t, control.AcknowledgePlacementWorker(ctx, ack))
	out, err = control.ObservePlacement(ctx, moving.ID)
	require.NoError(t, err)
	require.Equal(t, "fixture-deployment", out.WorkerAck.WorkerRevision)
	require.NoError(t, control.AcknowledgePlacementWorker(ctx, ack))
	observedAgain, err := control.ObservePlacement(ctx, moving.ID)
	require.NoError(t, err)
	require.Equal(t, out.WorkerAck.AcknowledgedAt, observedAgain.WorkerAck.AcknowledgedAt)
}

func TestOnEnabledChannelsSwapRetainsUnchangedProviders(t *testing.T) {
	var started, stopped atomic.Int32
	retained := &Channel{startTokenProvider: func() { started.Add(1) }, stopTokenProvider: func() { stopped.Add(1) }}
	retiredDone := make(chan struct{})
	retired := &Channel{stopTokenProvider: func() { close(retiredDone) }}
	svc := &ChannelService{}
	svc.onEnabledChannelsSwap([]*Channel{retained, retired}, []*Channel{retained})
	select {
	case <-retiredDone:
	case <-time.After(time.Second):
		t.Fatal("retired provider was not stopped")
	}
	require.Zero(t, started.Load(), "retained provider must not start twice")
	require.Zero(t, stopped.Load(), "retained provider must not be stopped")
}
