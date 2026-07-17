package biz

import (
	"context"
	"fmt"
	"strings"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/apikeyprofiletemplate"
	"github.com/looplj/axonhub/internal/objects"
)

func materializeAccessGroupProfile(groupName string, profile *objects.APIKeyProfile) (*objects.APIKeyProfiles, error) {
	cloned := profile.Clone()
	if cloned == nil {
		cloned = &objects.APIKeyProfile{}
	}
	if strings.TrimSpace(cloned.Name) == "" {
		cloned.Name = strings.TrimSpace(groupName)
	}
	profiles := &objects.APIKeyProfiles{ActiveProfile: cloned.Name, Profiles: []objects.APIKeyProfile{*cloned}}
	if err := validateProfileNames(profiles.Profiles); err != nil {
		return nil, err
	}
	if err := validateActiveProfile(profiles.ActiveProfile, profiles.Profiles); err != nil {
		return nil, err
	}
	if err := validateProfileFilters(profiles.Profiles); err != nil {
		return nil, err
	}
	if err := validateProfileQuota(profiles.Profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

func createAccessGroupRevision(ctx context.Context, client *ent.Client, group *ent.APIKeyProfileTemplate) error {
	bypassCtx := authz.WithSystemBypass(ctx, "access-group-revision")
	create := client.APIKeyProfileTemplateRevision.Create().
		SetProjectID(group.ProjectID).
		SetTemplateID(group.ID).
		SetRevision(group.Revision).
		SetName(group.Name).
		SetDescription(group.Description).
		SetProfile(group.Profile)
	if actor, ok := contexts.GetUser(ctx); ok && actor != nil {
		create.SetCreatedByUserID(actor.ID)
	}
	if _, err := create.Save(bypassCtx); err != nil {
		return fmt.Errorf("failed to save access group revision: %w", err)
	}
	return nil
}

// lockAccessGroup serializes every linked-key creation and policy mutation on
// the Access Group row. SQLite already serializes writers and does not support
// SELECT FOR UPDATE; the CAS in updateAccessGroupPolicy remains the backstop.
func lockAccessGroup(ctx context.Context, client *ent.Client, id int) (*ent.APIKeyProfileTemplate, error) {
	query := client.APIKeyProfileTemplate.Query().Where(apikeyprofiletemplate.IDEQ(id))
	var (
		group *ent.APIKeyProfileTemplate
		err   error
	)
	if client.Driver().Dialect() != dialect.SQLite {
		group, err = query.Modify(func(selector *sql.Selector) {
			selector.ForUpdate()
		}).Only(ctx)
	} else {
		group, err = query.Only(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to lock access group: %w", err)
	}
	return group, nil
}

type accessGroupPolicyPatch struct {
	Name        *string
	Description *string
	Profile     *objects.APIKeyProfile
	Visibility  *bool
}

type accessGroupPolicyMutation func(*ent.APIKeyProfileTemplate) (accessGroupPolicyPatch, error)

// updateAccessGroupPolicy is the single dispatcher for Access Group policy
// mutations. The caller derives its patch from the locked head, so a stale
// pre-transaction read can never overwrite an unrelated concurrent change.
// It atomically advances routing revision and refreshes every linked key. A
// visibility-only update deliberately does not advance the routing revision.
func updateAccessGroupPolicy(
	ctx context.Context,
	client *ent.Client,
	id int,
	mutate accessGroupPolicyMutation,
) (*ent.APIKeyProfileTemplate, []*ent.APIKey, error) {
	group, err := lockAccessGroup(ctx, client, id)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get access group: %w", err)
	}
	patch, err := mutate(group)
	if err != nil {
		return nil, nil, err
	}

	newName := group.Name
	if patch.Name != nil {
		newName = strings.TrimSpace(*patch.Name)
		if newName == "" {
			return nil, nil, fmt.Errorf("access group name is required")
		}
	}
	newDescription := group.Description
	if patch.Description != nil {
		newDescription = strings.TrimSpace(*patch.Description)
	}
	newProfile := group.Profile
	if patch.Profile != nil {
		newProfile = patch.Profile.Clone()
	}

	routingChanged := newName != group.Name || newDescription != group.Description || patch.Profile != nil
	update := client.APIKeyProfileTemplate.Update().
		Where(
			apikeyprofiletemplate.IDEQ(group.ID),
			apikeyprofiletemplate.RevisionEQ(group.Revision),
		).
		SetName(newName).
		SetDescription(newDescription).
		SetProfile(newProfile)
	if patch.Visibility != nil {
		update.SetSelfServiceVisible(*patch.Visibility)
	}
	if routingChanged {
		update.SetRevision(group.Revision + 1)
	}

	updatedCount, err := update.Save(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to update access group: %w", err)
	}
	if updatedCount != 1 {
		return nil, nil, fmt.Errorf("access group revision changed concurrently")
	}
	updated, err := client.APIKeyProfileTemplate.Get(ctx, group.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to reload updated access group: %w", err)
	}
	if !routingChanged {
		return updated, nil, nil
	}

	profiles, err := materializeAccessGroupProfile(updated.Name, updated.Profile)
	if err != nil {
		return nil, nil, err
	}
	keys, err := client.APIKey.Query().Where(
		apikey.ProjectIDEQ(updated.ProjectID),
		apikey.ProfileModeEQ(apikey.ProfileModeAccessGroup),
		apikey.AccessGroupIDEQ(updated.ID),
	).All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load attached API keys: %w", err)
	}
	for _, key := range keys {
		if _, err := client.APIKey.UpdateOneID(key.ID).
			SetProfiles(profiles).
			SetAccessGroupRevision(updated.Revision).
			Save(ctx); err != nil {
			return nil, nil, fmt.Errorf("failed to materialize access group on API key %d: %w", key.ID, err)
		}
	}
	if err := createAccessGroupRevision(ctx, client, updated); err != nil {
		return nil, nil, err
	}
	return updated, keys, nil
}
