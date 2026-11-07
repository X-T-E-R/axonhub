package biz

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/oauth"
)

type PlacementChannel struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type PlacementPlan struct {
	Version    int64              `json:"placement_version"`
	ChannelID  int                `json:"channel_id"`
	From       string             `json:"from"`
	To         string             `json:"to"`
	Channels   []PlacementChannel `json:"channels"`
	Identities map[int]string     `json:"identities,omitempty"`
}

type PlacementSaveInput struct {
	ChannelID       int    `json:"channel_id"`
	Target          string `json:"target"`
	ExpectedVersion int64  `json:"expected_version"`
	RequestID       string `json:"request_id"`
	ChannelIDs      []int  `json:"channel_ids"`
}

type PlacementNodeReadback struct {
	Node    string `json:"node"`
	Version int64  `json:"placement_version"`
	Ready   bool   `json:"ready"`
}

type PlacementObservation struct {
	Version     int64                  `json:"placement_version"`
	Owner       string                 `json:"owner"`
	Change      *PlacementChange       `json:"change,omitempty"`
	Target      *PlacementNodeReadback `json:"target,omitempty"`
	TargetError string                 `json:"target_error,omitempty"`
	WorkerAck   *PlacementWorkerAck    `json:"worker_ack,omitempty"`
}

func requirePlacementWrite(ctx context.Context) error {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return err
	}
	principal, _ := authz.GetPrincipal(ctx)
	if principal.IsSystem() || principal.IsTest() {
		return nil
	}
	user, ok := contexts.GetUser(ctx)
	if !ok {
		return errors.New("channel placement requires an administrator")
	}
	if user.IsOwner || slices.Contains(user.Scopes, string(scopes.ScopeWriteChannels)) {
		return nil
	}
	for _, role := range user.Edges.Roles {
		if role.IsSystemRole() && slices.Contains(role.Scopes, string(scopes.ScopeWriteChannels)) {
			return nil
		}
	}
	return errors.New("channel placement requires system-level write_channels")
}

func placementCredentialKeys(ch *ent.Channel) []string {
	keys := ch.Credentials.GetAllAPIKeys()
	creds := ch.Credentials.OAuth
	if creds == nil && ch.Credentials.IsOAuth() {
		creds, _ = oauth.ParseCredentialsJSON(ch.Credentials.APIKey)
	}
	if creds != nil {
		keys = append(keys, creds.AccessToken, creds.RefreshToken)
	}
	if gcp := ch.Credentials.GCP; gcp != nil && gcp.JSONData != "" {
		keys = append(keys, gcp.JSONData)
	}
	if ch.Type == channel.TypeAntigravity {
		if refreshToken, _, ok := strings.Cut(ch.Credentials.APIKey, "|"); ok {
			keys = append(keys, refreshToken)
		}
	}
	return slices.DeleteFunc(keys, func(key string) bool { return key == "" })
}

// Preview moves the transitive credential identity group, including disabled and
// archived peers. Secrets are compared in memory and never returned or persisted.
func (svc *ChannelService) PreviewPlacement(ctx context.Context, channelID int, target string) (*PlacementPlan, error) {
	if err := requirePlacementWrite(ctx); err != nil {
		return nil, err
	}
	if !svc.node.Enabled || !svc.node.IsControl() {
		return nil, errors.New("cluster control node required")
	}
	if _, ok := svc.node.Nodes[target]; !ok {
		return nil, fmt.Errorf("execution machine %q is not registered", target)
	}
	_, state, err := readChannelPlacement(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	bindings := svc.node.AtPlacement(state.PlacementSnapshot).Bindings()
	binding := bindings[channelID]
	all, err := observationPrimaryClient(svc.db).Channel.Query().Order(ent.Asc(channel.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	identities := map[string]bool{}
	secrets := map[string]bool{}
	selected := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for _, ch := range all {
			if selected[ch.ID] {
				continue
			}
			keys := placementCredentialKeys(ch)
			peer := bindings[ch.ID]
			shares := ch.ID == channelID || identities[peer.Identity]
			for _, key := range keys {
				shares = shares || secrets[key]
			}
			if !shares {
				continue
			}
			if peer.Identity == "" && (ch.Status == channel.StatusEnabled || (ch.ID == channelID && ch.Status != channel.StatusDisabled)) {
				return nil, fmt.Errorf("disable channel %d before assigning its execution machine", ch.ID)
			}
			selected[ch.ID] = true
			if peer.Identity != "" {
				identities[peer.Identity] = true
			}
			for _, key := range keys {
				secrets[key] = true
			}
			changed = true
		}
	}
	if !selected[channelID] {
		return nil, errors.New("channel not found")
	}
	owner, identity := binding.Owner, binding.Identity
	for _, ch := range all {
		peer := bindings[ch.ID]
		if !selected[ch.ID] || peer.Identity == "" {
			continue
		}
		if identity == "" {
			owner, identity = peer.Owner, peer.Identity
		}
		if peer.Owner != owner {
			return nil, fmt.Errorf("shared credential channel %d has inconsistent owner", ch.ID)
		}
	}
	if binding.Identity == "" && owner != "" && target != owner {
		return nil, fmt.Errorf("shared credentials already use %q; assign this channel there before moving the group", owner)
	}
	if identity == "" {
		identity = uuid.NewString()
	}
	plan := &PlacementPlan{Version: state.Version, ChannelID: channelID, From: binding.Owner, To: target, Channels: []PlacementChannel{}, Identities: map[int]string{}}
	for _, ch := range all {
		if selected[ch.ID] {
			plan.Channels = append(plan.Channels, PlacementChannel{ID: ch.ID, Name: ch.Name, Status: ch.Status.String()})
			plan.Identities[ch.ID] = cmp.Or(bindings[ch.ID].Identity, identity)
		}
	}
	return plan, nil
}

// apply updates the identity registrations and their owner in the same snapshot.
func (plan *PlacementPlan) apply(snapshot *cluster.PlacementSnapshot, existing cluster.Config) error {
	for _, peer := range plan.Channels {
		identity := plan.Identities[peer.ID]
		current := existing.Binding(peer.ID).Identity
		if identity == "" || (current != "" && current != identity) {
			return fmt.Errorf("placement identity mismatch for channel %d", peer.ID)
		}
		if current == "" {
			snapshot.ChannelIdentities[peer.ID] = identity
		}
		snapshot.Owners[identity] = plan.To
	}
	snapshot.Version++
	return nil
}

// Prepare constructs target providers without starting OAuth loops or issuing an
// upstream request. It verifies the target node, primary data and identity map.
func (svc *ChannelService) PreparePlacement(ctx context.Context, plan PlacementPlan) (*PlacementNodeReadback, error) {
	if err := requirePlacementWrite(ctx); err != nil {
		return nil, err
	}
	if !svc.node.Enabled || svc.node.NodeID != plan.To {
		return nil, errors.New("placement target node mismatch")
	}
	_, state, err := readChannelPlacement(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	if state.Version != plan.Version {
		return nil, errors.New("placement changed; refresh the preview")
	}
	existing := svc.node.AtPlacement(state.PlacementSnapshot)
	if err := plan.apply(&state.PlacementSnapshot, existing); err != nil {
		return nil, err
	}
	prepared := &ChannelService{AbstractService: &AbstractService{db: observationPrimaryClient(svc.db)}, node: svc.node.AtPlacement(state.PlacementSnapshot), SystemService: svc.SystemService, httpClient: svc.httpClient}
	for _, peer := range plan.Channels {
		ch, err := observationPrimaryClient(svc.db).Channel.Get(ctx, peer.ID)
		if err != nil {
			return nil, err
		}
		current := existing.Binding(peer.ID)
		if current.Identity == "" && (ch.Status == channel.StatusEnabled || (peer.ID == plan.ChannelID && ch.Status != channel.StatusDisabled)) {
			return nil, fmt.Errorf("disable channel %d before assigning its execution machine", ch.ID)
		}
		if ch.Status != channel.StatusEnabled || current.Owner == plan.To {
			continue
		}
		candidate, err := prepared.buildChannelWithOutbounds(ch)
		if err != nil {
			return nil, fmt.Errorf("target channel %d: %w", ch.ID, err)
		}
		prepared.cleanupSwappedChannels([]*Channel{candidate})
	}
	return &PlacementNodeReadback{Node: svc.node.NodeID, Version: plan.Version, Ready: true}, nil
}

func (svc *ChannelService) callPlacementNode(ctx context.Context, node, path string, input, output any) error {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return cluster.CallNode(callCtx, svc.node, node, path, input, output)
}

func (svc *ChannelService) SavePlacement(ctx context.Context, input PlacementSaveInput) (*PlacementObservation, error) {
	if err := requirePlacementWrite(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 {
		return nil, errors.New("request_id is required (maximum 128 characters)")
	}
	_, prior, err := readChannelPlacement(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	if change, exists := prior.Changes[input.ChannelID]; exists && change.RequestID == input.RequestID {
		if change.To != input.Target {
			return nil, errors.New("request_id already belongs to another target")
		}
		return svc.ObservePlacement(ctx, input.ChannelID)
	}
	plan, err := svc.PreviewPlacement(ctx, input.ChannelID, input.Target)
	if err != nil {
		return nil, err
	}
	if plan.Version != input.ExpectedVersion {
		return nil, errors.New("placement changed; refresh the preview")
	}
	confirmed := slices.Clone(input.ChannelIDs)
	slices.Sort(confirmed)
	actual := make([]int, 0, len(plan.Channels))
	for _, peer := range plan.Channels {
		actual = append(actual, peer.ID)
	}
	if !slices.Equal(confirmed, actual) {
		return nil, errors.New("affected channels changed; refresh and confirm the preview")
	}
	if plan.From == plan.To {
		return svc.ObservePlacement(ctx, input.ChannelID)
	}
	var target *PlacementNodeReadback
	if plan.To == svc.node.NodeID {
		target, err = svc.PreparePlacement(ctx, *plan)
	} else {
		target = &PlacementNodeReadback{}
		err = svc.callPlacementNode(ctx, plan.To, "/__axon/admin/placement/prepare", plan, target)
	}
	if err != nil {
		return nil, fmt.Errorf("target preparation failed; current owner unchanged: %w", err)
	}
	if !target.Ready || target.Node != plan.To || target.Version != plan.Version {
		return nil, errors.New("target is not ready; current owner unchanged")
	}
	primary := &AbstractService{db: observationPrimaryClient(svc.db)}
	var committed cluster.PlacementSnapshot
	err = primary.RunInTransaction(ent.NewContext(ctx, primary.db), func(txCtx context.Context) error {
		client := primary.entFromContext(txCtx)
		row, state, err := readChannelPlacement(txCtx, client)
		if err != nil {
			return err
		}
		if state.Version != input.ExpectedVersion {
			return errors.New("placement changed; refresh the preview")
		}
		if err := plan.apply(&state.PlacementSnapshot, svc.node.AtPlacement(state.PlacementSnapshot)); err != nil {
			return err
		}
		change := PlacementChange{RequestID: input.RequestID, Version: state.Version, From: plan.From, To: plan.To, SavedAt: time.Now().UTC()}
		for _, peer := range plan.Channels {
			change.ChannelIDs = append(change.ChannelIDs, peer.ID)
		}
		for _, id := range change.ChannelIDs {
			state.Changes[id] = change
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		systemCtx := placementSystemContext(txCtx)
		if row == nil {
			_, err = client.System.Create().SetKey(channelPlacementSystemKey).SetValue(string(raw)).Save(systemCtx)
		} else {
			var count int
			count, err = client.System.Update().Where(system.IDEQ(row.ID), system.ValueEQ(row.Value)).SetValue(string(raw)).Save(systemCtx)
			if err == nil && count != 1 {
				err = errors.New("placement changed; refresh the preview")
			}
		}
		if err != nil {
			return err
		}
		committed = state.PlacementSnapshot
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("placement save failed; observe current owner before retrying: %w", err)
	}
	svc.node.ApplyPlacement(committed)
	svc.notifyPlacementReload()
	// A source outage never blocks a committed online change. Cache cleanup and
	// existing streams finish on their ordinary lifecycle, without inference replay.
	return svc.observePlacement(ctx, input.ChannelID, true)
}

type PlacementReadbackInput struct {
	ChannelIDs []int `json:"channel_ids"`
	Version    int64 `json:"placement_version"`
	Refresh    bool  `json:"refresh"`
}

func (svc *ChannelService) ReadPlacementOnNode(ctx context.Context, input PlacementReadbackInput) (*PlacementNodeReadback, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	if input.Refresh {
		if err := requirePlacementWrite(ctx); err != nil {
			return nil, err
		}
		if err := svc.enabledChannelsCache.Load(ctx, false); err != nil {
			return nil, err
		}
	}
	version := svc.loadedPlacementVersion.Load()
	ready := version >= input.Version
	for _, id := range input.ChannelIDs {
		ch, err := observationPrimaryClient(svc.db).Channel.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if !svc.node.Owns(id) {
			ready = false
		}
		if ch.Status == channel.StatusEnabled {
			loaded := svc.GetEnabledChannel(id)
			if loaded == nil || loaded.RemoteOwner != "" {
				ready = false
			}
		}
	}
	return &PlacementNodeReadback{Node: svc.node.NodeID, Version: version, Ready: ready}, nil
}

func (svc *ChannelService) ObservePlacement(ctx context.Context, channelID int) (*PlacementObservation, error) {
	return svc.observePlacement(ctx, channelID, false)
}

func (svc *ChannelService) observePlacement(ctx context.Context, channelID int, refresh bool) (*PlacementObservation, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	_, state, err := readChannelPlacement(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	config := svc.node.AtPlacement(state.PlacementSnapshot)
	out := &PlacementObservation{Version: state.Version, Owner: config.Owner(channelID)}
	out.WorkerAck, err = readPlacementWorkerAck(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	change, changed := state.Changes[channelID]
	if !changed {
		return out, nil
	}
	out.Change = &change
	readback := PlacementReadbackInput{ChannelIDs: change.ChannelIDs, Version: change.Version, Refresh: refresh}
	if change.To == svc.node.NodeID {
		out.Target, err = svc.ReadPlacementOnNode(ctx, readback)
	} else {
		out.Target = &PlacementNodeReadback{}
		err = svc.callPlacementNode(ctx, change.To, "/__axon/admin/placement/readback", readback, out.Target)
	}
	if err != nil {
		out.Target = nil
		out.TargetError = "target_unavailable"
	}
	return out, nil
}

func (svc *ChannelService) AcknowledgePlacementWorker(ctx context.Context, ack PlacementWorkerAck) error {
	user, ok := contexts.GetUser(ctx)
	if !ok || !user.IsOwner {
		return errors.New("owner access required")
	}
	if strings.TrimSpace(ack.WorkerRevision) == "" || len(ack.WorkerRevision) > 128 {
		return errors.New("worker_revision is required (maximum 128 characters)")
	}
	primary := &AbstractService{db: observationPrimaryClient(svc.db)}
	return primary.RunInTransaction(ent.NewContext(ctx, primary.db), func(txCtx context.Context) error {
		client := primary.entFromContext(txCtx)
		row, state, err := readChannelPlacement(txCtx, client)
		if err != nil {
			return err
		}
		if row == nil || ack.Version != state.Version {
			return errors.New("Worker acknowledgement does not match effective placement version")
		}
		// Serialize this acknowledgement against concurrent placement saves.
		count, err := client.System.Update().Where(system.IDEQ(row.ID), system.ValueEQ(row.Value)).SetValue(row.Value).Save(placementSystemContext(txCtx))
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.New("placement changed; synchronize Worker again")
		}
		previous, err := readPlacementWorkerAck(txCtx, client)
		if err != nil {
			return err
		}
		if previous != nil && previous.Version == ack.Version && previous.WorkerRevision == ack.WorkerRevision {
			return nil
		}
		ack.AcknowledgedAt = time.Now().UTC()
		raw, err := json.Marshal(ack)
		if err != nil {
			return err
		}
		return client.System.Create().SetKey(channelPlacementWorkerAckKey).SetValue(string(raw)).OnConflictColumns(system.FieldKey).UpdateValue().Exec(placementSystemContext(txCtx))
	})
}
