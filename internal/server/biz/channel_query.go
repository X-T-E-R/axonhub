package biz

import (
	"context"
	"fmt"

	"entgo.io/contrib/entgql"
	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/scopes"
)

// QueryChannelsInput represents the input for querying channels with additional filters.
type QueryChannelsInput struct {
	After                *entgql.Cursor[int]
	First                *int
	Before               *entgql.Cursor[int]
	Last                 *int
	OrderBy              *ent.ChannelOrder
	Where                *ent.ChannelWhereInput
	HasTag               *string
	Model                *string
	ExecutionMachine     *string
	PaginateModelMatches *bool
}

// QueryChannels queries channels with the specified input parameters, including model filtering.
func (svc *ChannelService) QueryChannels(ctx context.Context, input QueryChannelsInput) (*ent.ChannelConnection, error) {
	// Build the base query
	var (
		query = svc.entFromContext(ctx).Channel.Query()
		err   error
	)

	// Apply standard filters
	if input.Where != nil {
		query, err = input.Where.Filter(query)
		if err != nil {
			return nil, err
		}
	}

	if input.HasTag != nil && *input.HasTag != "" {
		query = query.Where(func(s *sql.Selector) {
			s.Where(sqljson.ValueContains("tags", *input.HasTag))
		})
	}

	if input.ExecutionMachine != nil {
		if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
			return nil, err
		}
		machine := *input.ExecutionMachine
		if _, registered := svc.node.Nodes[machine]; !svc.node.Enabled || !registered {
			return nil, fmt.Errorf("execution machine %q is not registered", machine)
		}
		var ids []int
		for id, binding := range svc.node.Bindings() {
			if binding.Owner == machine {
				ids = append(ids, id)
			}
		}
		query.Where(channel.IDIn(ids...))
	}

	if input.Model != nil && *input.Model != "" {
		paginate := input.ExecutionMachine != nil || (input.PaginateModelMatches != nil && *input.PaginateModelMatches)
		matching, err := matchingModelChannels(ctx, query.Clone(), *input.Model, input.OrderBy)
		if err != nil {
			return nil, err
		}
		if !paginate {
			// Keep the unpaginated model search contract for existing callers.
			return svc.buildConnectionInMemory(matching, input.OrderBy), nil
		}
		ids := make([]int, 0, len(matching))
		for _, ch := range matching {
			ids = append(ids, ch.ID)
		}
		query.Where(channel.IDIn(ids...))
	}

	conn, err := query.Paginate(ctx, input.After, input.First, input.Before, input.Last, ent.WithChannelOrder(input.OrderBy))
	if err != nil {
		return nil, err
	}
	normalizeChannelConnectionSettingsForRead(conn)
	return conn, nil
}

// Model support includes aliases and mapping rules evaluated by Channel. Select
// all matches before applying the same cursor pagination as the ordinary query.
func matchingModelChannels(ctx context.Context, query *ent.ChannelQuery, model string, order *ent.ChannelOrder) ([]*ent.Channel, error) {
	if order != nil {
		query.Order(order.ToOrderOption())
	}
	channels, err := query.All(ctx)
	if err != nil {
		return nil, err
	}
	var matching []*ent.Channel
	for _, ch := range channels {
		candidate := Channel{Channel: ch}
		if candidate.IsModelSupported(model) {
			matching = append(matching, ch)
		}
	}
	return matching, nil
}

// buildConnectionInMemory builds a relay-style connection from filtered channels.
func (svc *ChannelService) buildConnectionInMemory(
	channels []*ent.Channel,
	order *ent.ChannelOrder,
) *ent.ChannelConnection {
	conn := &ent.ChannelConnection{
		Edges:    []*ent.ChannelEdge{},
		PageInfo: ent.PageInfo{},
	}

	// Handle empty result
	if len(channels) == 0 {
		conn.TotalCount = 0
		return conn
	}

	// Return all channels without pagination
	conn.Edges = make([]*ent.ChannelEdge, len(channels))
	for i, ch := range channels {
		ch.Settings = normalizeChannelSettingsForRead(ch.Settings)
		conn.Edges[i] = ch.ToEdge(order)
	}

	conn.PageInfo.HasNextPage = false
	conn.PageInfo.HasPreviousPage = false

	conn.TotalCount = len(channels)
	if len(conn.Edges) > 0 {
		conn.PageInfo.StartCursor = &conn.Edges[0].Cursor
		conn.PageInfo.EndCursor = &conn.Edges[len(conn.Edges)-1].Cursor
	}

	return conn
}

func normalizeChannelConnectionSettingsForRead(conn *ent.ChannelConnection) {
	if conn == nil {
		return
	}

	for _, edge := range conn.Edges {
		if edge == nil || edge.Node == nil {
			continue
		}
		edge.Node.Settings = normalizeChannelSettingsForRead(edge.Node.Settings)
	}
}
