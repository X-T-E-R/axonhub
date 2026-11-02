package backup

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
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
