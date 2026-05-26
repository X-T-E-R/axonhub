package api

import (
	"context"
	"slices"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/scopes"
)

func currentUserCanReadProjectRequests(ctx context.Context, projectID int) bool {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return false
	}

	return userCanReadProjectRequests(currentUser, projectID)
}

func userCanReadProjectRequests(user *ent.User, projectID int) bool {
	if user.IsOwner {
		return true
	}

	requiredScope := string(scopes.ScopeReadRequests)
	if slices.Contains(user.Scopes, requiredScope) {
		return true
	}

	for _, role := range user.Edges.Roles {
		if role == nil {
			continue
		}

		if role.ProjectID == nil && slices.Contains(role.Scopes, requiredScope) {
			return true
		}
		if role.ProjectID != nil && *role.ProjectID == projectID && slices.Contains(role.Scopes, requiredScope) {
			return true
		}
	}

	for _, membership := range user.Edges.ProjectUsers {
		if membership == nil || membership.ProjectID != projectID {
			continue
		}

		if membership.IsOwner || slices.Contains(membership.Scopes, requiredScope) {
			return true
		}
	}

	return false
}
