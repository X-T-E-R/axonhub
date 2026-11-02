package backup

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func createPortalBackupOwner(t *testing.T, client *ent.Client, ctx context.Context, email string, isOwner bool, accessMode user.AccessMode) *ent.User {
	t.Helper()

	owner, err := client.User.Create().
		SetEmail(email).
		SetPassword("password").
		SetIsOwner(isOwner).
		SetStatus(user.StatusActivated).
		SetAccessMode(accessMode).
		Save(ctx)
	require.NoError(t, err)

	return owner
}

func createPortalBackupKey(t *testing.T, client *ent.Client, ctx context.Context, owner *ent.User, project *ent.Project, name, key string, portalManaged bool) *ent.APIKey {
	t.Helper()

	apiKey, err := client.APIKey.Create().
		SetKey(key).
		SetName(name).
		SetType(apikey.TypePersonal).
		SetStatus(apikey.StatusEnabled).
		SetScopes([]string{"read_channels", "write_requests"}).
		SetProfiles(&objects.APIKeyProfiles{
			ActiveProfile: "personal",
			Profiles: []objects.APIKeyProfile{
				{Name: "personal", ModelIDs: []string{"gpt-4"}},
			},
		}).
		SetUserID(owner.ID).
		SetProjectID(project.ID).
		SetPortalManaged(portalManaged).
		Save(ctx)
	require.NoError(t, err)

	return apiKey
}

func TestBackupService_Backup_PortalMetadata(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	portalOwner := createPortalBackupOwner(t, client, ctx, "portal@example.com", false, user.AccessModePortal)
	project := createBackupTestProject(t, client, ctx, "Portal Project", "Portal project")
	managed := createPortalBackupKey(t, client, ctx, portalOwner, project, "Portal key", "sk-portal-backup", true)

	settings := objects.PortalSettings{
		Announcements: []*objects.PortalDocument{
			{Slug: "draft", Title: "Draft", Markdown: "draft body", Published: false, Pinned: true, Order: 1},
		},
		Guides: []*objects.PortalDocument{
			{Slug: "guide", Title: "Guide", Markdown: "guide body", Published: true, Order: 2},
		},
	}
	settingsJSON, err := json.Marshal(settings)
	require.NoError(t, err)
	client.System.Create().
		SetKey(biz.SystemKeyPortalSettings).
		SetValue(string(settingsJSON)).
		SaveX(ctx)

	data, err := service.Backup(ctx, BackupOptions{
		IncludeAPIKeys:  true,
		IncludeProjects: true,
	})
	require.NoError(t, err)

	var backupData BackupData
	require.NoError(t, json.Unmarshal(data, &backupData))
	require.Len(t, backupData.APIKeys, 1)
	require.Equal(t, managed.Key, backupData.APIKeys[0].Key)
	require.True(t, backupData.APIKeys[0].PortalManaged)
	require.Equal(t, portalOwner.Email, backupData.APIKeys[0].OwnerEmail)
	require.Equal(t, project.Name, backupData.APIKeys[0].ProjectName)

	require.Len(t, backupData.PortalOwners, 1)
	require.Equal(t, portalOwner.Email, backupData.PortalOwners[0].Email)
	require.Equal(t, string(user.AccessModePortal), backupData.PortalOwners[0].AccessMode)
	require.Equal(t, settings, *backupData.PortalSettings)
}

func TestResolvePortalOwner(t *testing.T) {
	client, _, ctx := setupBackupTest(t)
	defer client.Close()

	portalOwner := createPortalBackupOwner(t, client, ctx, "portal-owner@example.com", false, user.AccessModePortal)

	tests := []struct {
		name      string
		ownerMail string
		project   string
		keyType   apikey.Type
		wantID    int
	}{
		{
			name:      "activated non-owner",
			ownerMail: portalOwner.Email,
			project:   "Portal Project",
			keyType:   apikey.TypePersonal,
			wantID:    portalOwner.ID,
		},
		{
			name:      "owner identity rejected",
			ownerMail: "test@example.com",
			project:   "Portal Project",
			keyType:   apikey.TypePersonal,
		},
		{
			name:      "missing owner rejected",
			ownerMail: "missing@example.com",
			project:   "Portal Project",
			keyType:   apikey.TypePersonal,
		},
		{
			name:      "empty owner reference rejected",
			ownerMail: "",
			project:   "Portal Project",
			keyType:   apikey.TypePersonal,
		},
		{
			name:      "empty project reference rejected",
			ownerMail: portalOwner.Email,
			project:   "",
			keyType:   apikey.TypePersonal,
		},
		{
			name:      "non personal key rejected",
			ownerMail: portalOwner.Email,
			project:   "Portal Project",
			keyType:   apikey.TypeUser,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ownerID, err := resolvePortalOwner(ctx, client, &BackupAPIKey{
				APIKey:      ent.APIKey{Type: tt.keyType},
				ProjectName: tt.project,
				OwnerEmail:  tt.ownerMail,
			})
			if tt.wantID != 0 {
				require.NoError(t, err)
				require.Equal(t, tt.wantID, ownerID)
				return
			}

			require.Error(t, err)
			require.Zero(t, ownerID)
		})
	}
}

func TestBackupService_Restore_PortalManagedAPIKeyUsesOwnerAndDisables(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	recoveryAdmin, ok := contexts.GetUser(ctx)
	require.True(t, ok)
	portalOwner := createPortalBackupOwner(t, client, ctx, "portal-restore@example.com", false, user.AccessModePortal)
	project := createBackupTestProject(t, client, ctx, "Portal Restore Project", "Portal project")

	backupData := BackupData{
		Version: BackupVersion,
		PortalOwners: []*BackupPortalOwner{{
			Email:      portalOwner.Email,
			AccessMode: string(user.AccessModePortal),
		}},
		APIKeys: []*BackupAPIKey{{
			APIKey: ent.APIKey{
				Key:           "sk-portal-restore",
				Name:          "Restored portal key",
				Type:          apikey.TypePersonal,
				Status:        apikey.StatusEnabled,
				PortalManaged: true,
				Scopes:        []string{"read_channels", "write_requests"},
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "personal",
					Profiles:      []objects.APIKeyProfile{{Name: "personal", ModelIDs: []string{"gpt-4"}}},
				},
			},
			ProjectName: project.Name,
			OwnerEmail:  portalOwner.Email,
		}},
	}
	data, err := json.Marshal(backupData)
	require.NoError(t, err)

	require.NoError(t, service.Restore(ctx, data, RestoreOptions{
		IncludeAPIKeys:         true,
		APIKeyConflictStrategy: ConflictStrategyOverwrite,
	}))

	restored := client.APIKey.Query().Where(apikey.Key("sk-portal-restore")).OnlyX(ctx)
	require.True(t, restored.PortalManaged)
	require.Equal(t, portalOwner.ID, restored.UserID)
	require.NotEqual(t, recoveryAdmin.ID, restored.UserID)
	require.Equal(t, apikey.StatusDisabled, restored.Status)
}

func TestBackupService_Restore_RejectsPortalManagedProvenanceDowngrade(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	portalOwner := createPortalBackupOwner(t, client, ctx, "portal-downgrade@example.com", false, user.AccessModePortal)
	project := createBackupTestProject(t, client, ctx, "Portal Downgrade Project", "Portal project")
	existing := createPortalBackupKey(t, client, ctx, portalOwner, project, "Existing portal key", "sk-portal-downgrade", true)

	backupData := BackupData{
		Version: BackupVersion,
		APIKeys: []*BackupAPIKey{{
			APIKey: ent.APIKey{
				Key:           existing.Key,
				Name:          "Legacy-looking input",
				Type:          apikey.TypePersonal,
				Status:        apikey.StatusEnabled,
				PortalManaged: false,
				Scopes:        []string{"read_channels"},
			},
			ProjectName: project.Name,
		}},
	}
	data, err := json.Marshal(backupData)
	require.NoError(t, err)

	err = service.Restore(ctx, data, RestoreOptions{
		IncludeAPIKeys:         true,
		APIKeyConflictStrategy: ConflictStrategyOverwrite,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "provenance")

	unchanged := client.APIKey.Query().Where(apikey.Key(existing.Key)).OnlyX(ctx)
	require.True(t, unchanged.PortalManaged)
	require.Equal(t, existing.UserID, unchanged.UserID)
	require.Equal(t, existing.Name, unchanged.Name)
}

func TestBackupService_BackupRestore_PortalSettingsPreservesPublished(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	createBackupTestProject(t, client, ctx, "Portal Content Project", "Portal project")
	original := objects.PortalSettings{
		Announcements: []*objects.PortalDocument{
			{Slug: "draft-announcement", Title: "Draft", Markdown: "draft", Published: false, Order: 1},
		},
		Guides: []*objects.PortalDocument{
			{Slug: "published-guide", Title: "Guide", Markdown: "guide", Published: true, Order: 2},
		},
	}
	originalJSON, err := json.Marshal(original)
	require.NoError(t, err)
	client.System.Create().
		SetKey(biz.SystemKeyPortalSettings).
		SetValue(string(originalJSON)).
		SaveX(ctx)

	data, err := service.Backup(ctx, BackupOptions{IncludeProjects: true})
	require.NoError(t, err)
	var backupData BackupData
	require.NoError(t, json.Unmarshal(data, &backupData))
	require.NotNil(t, backupData.PortalSettings)
	require.False(t, backupData.PortalSettings.Announcements[0].Published)
	require.True(t, backupData.PortalSettings.Guides[0].Published)

	replacement := objects.PortalSettings{
		Announcements: []*objects.PortalDocument{
			{Slug: "draft-announcement", Title: "Draft", Markdown: "draft", Published: true, Order: 1},
		},
		Guides: []*objects.PortalDocument{
			{Slug: "published-guide", Title: "Guide", Markdown: "guide", Published: false, Order: 2},
		},
	}
	replacementJSON, err := json.Marshal(replacement)
	require.NoError(t, err)
	row := client.System.Query().Where(system.KeyEQ(biz.SystemKeyPortalSettings)).OnlyX(ctx)
	client.System.UpdateOne(row).SetValue(string(replacementJSON)).SaveX(ctx)

	require.NoError(t, service.Restore(ctx, data, RestoreOptions{
		IncludeProjects:         true,
		ProjectConflictStrategy: ConflictStrategyOverwrite,
	}))

	restoredRow := client.System.Query().Where(system.KeyEQ(biz.SystemKeyPortalSettings)).OnlyX(ctx)
	var restored objects.PortalSettings
	require.NoError(t, json.Unmarshal([]byte(restoredRow.Value), &restored))
	require.Equal(t, original, restored)
}

// F-01: a source ID collision must not change an existing enabled credential's
// authorization, even when no managed credential is present in the backup.
func TestPortalRestoreGroupChannelReferences(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()
	owner := createPortalBackupOwner(t, client, ctx, "restore-channels@example.test", false, user.AccessModePortal)
	allowed := createBackupTestChannel(t, client, ctx, "destination-approved", channel.TypeOpenai)
	unrelated := createBackupTestChannel(t, client, ctx, "destination-unrelated", channel.TypeOpenai)
	models := []string{"gpt-4"}
	group := client.Project.Create().SetName("Portal reference group").SetProfiles(&objects.ProjectProfiles{ActiveProfile: "active", Profiles: []objects.ProjectProfile{{Name: "active", ModelIDs: &models, ChannelIDs: []int{allowed.ID}}}}).SaveX(ctx)
	client.UserProject.Create().SetUserID(owner.ID).SetProjectID(group.ID).SaveX(ctx)
	key := createPortalBackupKey(t, client, ctx, owner, group, "Existing key", "ah-existing-channel-key", true)
	data := BackupData{Version: BackupVersion,
		Channels: []*BackupChannel{{Channel: ent.Channel{ID: unrelated.ID, Name: "missing-source-route"}}},
		// ModelIDs omitted intentionally: old backups preserve the existing model
		// limit, so existing managed keys must also trigger reference validation.
		Projects:       []*BackupProject{{Project: ent.Project{Name: group.Name, Status: group.Status, Profiles: &objects.ProjectProfiles{ActiveProfile: "active", Profiles: []objects.ProjectProfile{{Name: "active", ChannelIDs: []int{unrelated.ID}}}}}}},
		PortalSettings: &objects.PortalSettings{Guides: []*objects.PortalDocument{{Slug: "rollback", Title: "Rollback", Markdown: "must not persist"}}},
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	err = service.Restore(ctx, raw, RestoreOptions{IncludeProjects: true, ProjectConflictStrategy: ConflictStrategyOverwrite})
	require.ErrorContains(t, err, "cannot be resolved by source name")
	require.Equal(t, []int{allowed.ID}, client.Project.GetX(ctx, group.ID).GetActiveProfile().ChannelIDs)
	require.Equal(t, apikey.StatusEnabled, client.APIKey.GetX(ctx, key.ID).Status)
	require.False(t, client.System.Query().Where(system.KeyEQ(biz.SystemKeyPortalSettings)).ExistX(ctx), "earlier content writes must roll back")
	current, err := biz.RefreshAPIKeyAuthorization(ctx, client, key.Key)
	require.NoError(t, err)
	require.True(t, biz.KeyAllowsChannel(current, allowed.ID, allowed.Tags))
	require.False(t, biz.KeyAllowsChannel(current, unrelated.ID, unrelated.Tags))

	// The same source ID is valid when its manifest name resolves to a different
	// target ID. All profile channel IDs and the model limit survive the remap.
	data.Channels[0].Name = allowed.Name
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, service.Restore(ctx, raw, RestoreOptions{IncludeProjects: true, ProjectConflictStrategy: ConflictStrategyOverwrite}))
	restored := client.Project.GetX(ctx, group.ID).GetActiveProfile()
	require.Equal(t, []int{allowed.ID}, restored.ChannelIDs)
	require.Equal(t, models, *restored.ModelIDs)

	// An explicitly configured destination group needs the same protection even
	// before its first managed key is created; old backups retain its ModelIDs.
	emptyGroup := client.Project.Create().SetName("Portal group without keys").SetProfiles(group.Profiles).SaveX(ctx)
	data.PortalSettings = nil
	data.Projects[0].Name = emptyGroup.Name
	data.Channels[0].Name = "missing-source-route"
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	err = service.Restore(ctx, raw, RestoreOptions{IncludeProjects: true, ProjectConflictStrategy: ConflictStrategyOverwrite})
	require.ErrorContains(t, err, "cannot be resolved by source name")
	require.Equal(t, []int{allowed.ID}, client.Project.GetX(ctx, emptyGroup.ID).GetActiveProfile().ChannelIDs)

	// A legacy-only project still retains the previous partial-restore contract.
	data.Projects[0].Name = "Legacy reference group"
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, service.Restore(ctx, raw, RestoreOptions{IncludeProjects: true}))
	legacy := client.Project.Query().Where(project.NameEQ("Legacy reference group")).OnlyX(ctx)
	require.Equal(t, []int{unrelated.ID}, legacy.GetActiveProfile().ChannelIDs)
}

func TestPortalRestoreKeyChannelReferences(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()
	owner := createPortalBackupOwner(t, client, ctx, "restore-key-channels@example.test", false, user.AccessModePortal)
	group := createBackupTestProject(t, client, ctx, "Key reference group", "destination")
	allowed := createBackupTestChannel(t, client, ctx, "key-approved", channel.TypeOpenai)
	unrelated := createBackupTestChannel(t, client, ctx, "key-unrelated", channel.TypeOpenai)
	key := createPortalBackupKey(t, client, ctx, owner, group, "Original key", "ah-key-reference", true)
	original := &objects.APIKeyProfiles{ActiveProfile: "personal", Profiles: []objects.APIKeyProfile{{Name: "personal", ChannelIDs: []int{allowed.ID}}}}
	client.APIKey.UpdateOne(key).SetProfiles(original).SaveX(ctx)
	data := BackupData{Version: BackupVersion,
		Channels: []*BackupChannel{{Channel: ent.Channel{ID: unrelated.ID, Name: "missing-key-source"}}},
		APIKeys:  []*BackupAPIKey{{APIKey: ent.APIKey{Key: key.Key, Name: "Replacement key", Type: apikey.TypePersonal, PortalManaged: true, Status: apikey.StatusEnabled, Profiles: &objects.APIKeyProfiles{ActiveProfile: "personal", Profiles: []objects.APIKeyProfile{{Name: "personal", ChannelIDs: []int{unrelated.ID}}, {Name: "inactive", ChannelIDs: []int{unrelated.ID}}}}}, ProjectName: group.Name, OwnerEmail: owner.Email}},
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	err = service.Restore(ctx, raw, RestoreOptions{IncludeAPIKeys: true, APIKeyConflictStrategy: ConflictStrategyOverwrite})
	require.ErrorContains(t, err, "cannot be resolved by source name")
	require.Equal(t, "Original key", client.APIKey.GetX(ctx, key.ID).Name)
	require.Equal(t, original, client.APIKey.GetX(ctx, key.ID).Profiles)
	data.Channels[0].Name = allowed.Name
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, service.Restore(ctx, raw, RestoreOptions{IncludeAPIKeys: true, APIKeyConflictStrategy: ConflictStrategyOverwrite}))
	restored := client.APIKey.GetX(ctx, key.ID)
	for _, p := range restored.Profiles.Profiles {
		require.Equal(t, []int{allowed.ID}, p.ChannelIDs)
	}
	require.Equal(t, owner.ID, restored.UserID)
}

// F-02: a missing non-empty managed group reference is a failed transaction,
// while the legacy warning/skip contract remains unchanged.
func TestPortalRestoreMissingGroupRollsBack(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()
	owner := createPortalBackupOwner(t, client, ctx, "restore-missing-group@example.test", false, user.AccessModePortal)
	existing := createBackupTestProject(t, client, ctx, "Existing legacy group", "destination")
	data := BackupData{Version: BackupVersion, PortalOwners: []*BackupPortalOwner{{Email: owner.Email, AccessMode: "portal"}}, APIKeys: []*BackupAPIKey{
		{APIKey: ent.APIKey{Key: "ah-prior-legacy", Name: "Earlier restored key", Type: apikey.TypeUser, Status: apikey.StatusEnabled}, ProjectName: existing.Name},
		{APIKey: ent.APIKey{Key: "ah-missing-group", Name: "Managed missing group", Type: apikey.TypePersonal, Status: apikey.StatusEnabled, PortalManaged: true}, ProjectName: "Not provisioned", OwnerEmail: owner.Email},
	}}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	err = service.Restore(ctx, raw, RestoreOptions{IncludeAPIKeys: true, APIKeyConflictStrategy: ConflictStrategyOverwrite})
	require.ErrorContains(t, err, `group "Not provisioned" is unavailable`)
	require.Zero(t, client.APIKey.Query().CountX(ctx), "earlier inserts must roll back too")
	group := createBackupTestProject(t, client, ctx, "Not provisioned", "provisioned explicitly")
	require.NoError(t, service.Restore(ctx, raw, RestoreOptions{IncludeAPIKeys: true, APIKeyConflictStrategy: ConflictStrategyOverwrite}))
	key := client.APIKey.Query().Where(apikey.KeyEQ("ah-missing-group")).OnlyX(ctx)
	require.Equal(t, owner.ID, key.UserID)
	require.Equal(t, group.ID, key.ProjectID)
	require.True(t, key.PortalManaged)
	require.Equal(t, apikey.StatusDisabled, key.Status)
	data.PortalOwners = nil
	data.APIKeys = []*BackupAPIKey{{APIKey: ent.APIKey{Key: "ah-legacy-missing-group", Name: "Legacy missing group", Type: apikey.TypeUser}, ProjectName: "Still absent"}}
	raw, err = json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, service.Restore(ctx, raw, RestoreOptions{IncludeAPIKeys: true}))
	require.False(t, client.APIKey.Query().Where(apikey.KeyEQ("ah-legacy-missing-group")).ExistX(ctx))
}
