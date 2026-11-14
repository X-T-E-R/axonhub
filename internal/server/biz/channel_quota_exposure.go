package biz

import (
	"context"
	"strings"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
)

// ChannelQuotaExposureFilter limits the channels that may contribute to a
// downstream quota response. An empty Model matches any model that the client
// may use; ChannelID is an optional exact channel filter.
type ChannelQuotaExposureFilter struct {
	Model     string
	ChannelID *int
}

// AuthorizedQuotaChannels returns only enabled channels that the API key may
// use. Quota data is intentionally resolved from the same cached channel view
// used by the request router, rather than querying every channel in storage.
// This keeps the exposure boundary aligned with ordinary downstream model and
// channel authorization and avoids leaking disabled or unrelated channels.
func (svc *ChannelService) AuthorizedQuotaChannels(key *ent.APIKey, filter ChannelQuotaExposureFilter) []*Channel {
	if key == nil {
		return nil
	}

	modelID := strings.TrimSpace(filter.Model)
	channels := svc.GetEnabledChannels()
	out := make([]*Channel, 0, len(channels))
	for _, ch := range channels {
		if ch == nil || ch.Channel == nil || ch.Status != channel.StatusEnabled {
			continue
		}
		if filter.ChannelID != nil && ch.ID != *filter.ChannelID {
			continue
		}
		if !KeyAllowsChannel(key, ch.ID, ch.Tags) {
			continue
		}

		if modelID == "" {
			if channelHasAuthorizedQuotaModel(ch, key) {
				out = append(out, ch)
			}
			continue
		}
		if channelHasAuthorizedQuotaModel(ch, key, modelID) {
			out = append(out, ch)
		}
	}

	return out
}

// ModelQuotaChannels restricts an enabled, key-authorized channel set to the
// actual resolved connections for one public model. It reuses the model catalog
// resolver so model-association and API-key mapping rules match /v1/models.
func (svc *ModelService) ModelQuotaChannels(ctx context.Context, key *ent.APIKey, modelID string, channelID *int) ([]*Channel, error) {
	requestedModel := strings.TrimSpace(modelID)
	connections, err := svc.modelConnectionsForKey(ctx, key, requestedModel)
	if err != nil {
		return nil, err
	}
	connected := make(map[int]struct{}, len(connections))
	for _, connection := range connections {
		if connection != nil && connection.Channel != nil {
			connected[connection.Channel.ID] = struct{}{}
		}
	}
	out := make([]*Channel, 0, len(connected))
	for _, ch := range svc.channelService.GetEnabledChannels() {
		if ch == nil || ch.Channel == nil || ch.Status != channel.StatusEnabled || !KeyAllowsChannel(key, ch.ID, ch.Tags) {
			continue
		}
		if channelID != nil && ch.ID != *channelID {
			continue
		}
		if _, ok := connected[ch.ID]; ok {
			out = append(out, ch)
			continue
		}
		if IsRawHTTPChannel(ch.Channel) && ch.Settings.RawHTTP != nil && ch.Settings.RawHTTP.Resource == requestedModel && KeyAllowsModel(key, requestedModel) {
			out = append(out, ch)
		}
	}
	return out, nil
}

// channelHasAuthorizedQuotaModel reports whether a channel has at least one
// public model (or its raw HTTP resource) that the key may access. With a
// requested model, aliases and prefixes are checked through the channel's
// model-entry map so the exposure follows the same public identifiers as the
// model endpoint.
func channelHasAuthorizedQuotaModel(ch *Channel, key *ent.APIKey, requested ...string) bool {
	if ch == nil || ch.Channel == nil || key == nil {
		return false
	}

	requestedModel := ""
	if len(requested) > 0 {
		requestedModel = strings.TrimSpace(requested[0])
	}
	if requestedModel != "" && IsRawHTTPChannel(ch.Channel) && ch.Settings.RawHTTP != nil &&
		strings.TrimSpace(ch.Settings.RawHTTP.Resource) == requestedModel && KeyAllowsModel(key, requestedModel) {
		return true
	}

	for _, entry := range ch.GetModelEntries() {
		publicModel := strings.TrimSpace(entry.RequestModel)
		if publicModel == "" || !KeyAllowsModel(key, publicModel) {
			continue
		}
		if requestedModel == "" || publicModel == requestedModel {
			return true
		}
	}

	return false
}
