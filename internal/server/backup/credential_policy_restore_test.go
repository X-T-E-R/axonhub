package backup

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestBackupService_RestoreCredentialPolicy_MissingProxyRollsBackCleanly(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	preExisting := createBackupTestChannel(t, client, ctx, "pre-existing-channel", channel.TypeOpenai)

	backupData := BackupData{
		Version: BackupVersion,
		Projects: []*BackupProject{
			{
				Project: ent.Project{
					Name:        "rolled-back-project",
					Description: "should not persist when channel restore fails",
					Status:      project.StatusActive,
				},
			},
		},
		Channels: []*BackupChannel{
			{
				Channel: ent.Channel{
					Name:             "valid-before-failure",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-valid-1"}},
			},
			{
				Channel: ent.Channel{
					Name:             "missing-proxy-channel",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
					Settings: &objects.ChannelSettings{
						CredentialPolicies: []objects.ChannelCredentialPolicy{
							{
								ID:               objects.ChannelAPIKeyFingerprint("sk-missing-proxy"),
								Name:             "dangling-pinned",
								EgressMode:       objects.CredentialEgressModePinnedProxy,
								OutboundProxyID:  lo.ToPtr(999999),
								EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
							},
						},
					},
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-missing-proxy"}},
			},
		},
	}

	raw, err := json.Marshal(backupData)
	require.NoError(t, err)

	err = service.Restore(ctx, raw, RestoreOptions{
		IncludeChannels:         true,
		IncludeProjects:         true,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
		ProjectConflictStrategy: ConflictStrategyOverwrite,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")

	// Atomic transaction rollback: neither the earlier valid channel nor the project persisted.
	channels, err := client.Channel.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, preExisting.ID, channels[0].ID)

	existsProject, err := client.Project.Query().Where(project.Name("rolled-back-project")).Exist(ctx)
	require.NoError(t, err)
	require.False(t, existsProject)
}

func TestBackupService_RestoreCredentialPolicy_MaxCredentialsExceededRollsBackCleanly(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	proxySvc := biz.NewOutboundProxyService(biz.OutboundProxyServiceParams{Ent: client})
	proxyCap1, err := proxySvc.Create(ctx, biz.OutboundProxyCreateInput{
		Name:           "restore-cap-1",
		Kind:           biz.OutboundProxyKindStatic,
		ProxyURL:       "http://proxy.example.com:8080",
		Status:         biz.OutboundProxyStatusEnabled,
		MaxCredentials: lo.ToPtr(1),
	})
	require.NoError(t, err)

	existingKey := "sk-existing-bound"
	existingCh := client.Channel.Create().
		SetName("existing-bound-channel").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.com/v1").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{existingKey}}).
		SetSupportedModels([]string{"gpt-4o"}).
		SetDefaultTestModel("gpt-4o").
		SetSettings(&objects.ChannelSettings{
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:              objects.ChannelAPIKeyFingerprint(existingKey),
					Name:            "existing-pin",
					EgressMode:      objects.CredentialEgressModePinnedProxy,
					OutboundProxyID: lo.ToPtr(proxyCap1.ID),
				},
			},
		}).
		SaveX(ctx)

	// 1. Creating a new channel whose pinned policy pushes proxy usage above max_credentials=1 rolls back cleanly.
	createExcessData := BackupData{
		Version: BackupVersion,
		Channels: []*BackupChannel{
			{
				Channel: ent.Channel{
					Name:             "valid-peer-channel",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-peer"}},
			},
			{
				Channel: ent.Channel{
					Name:             "excess-create-channel",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
					Settings: &objects.ChannelSettings{
						CredentialPolicies: []objects.ChannelCredentialPolicy{
							{
								ID:              objects.ChannelAPIKeyFingerprint("sk-excess-2"),
								EgressMode:      objects.CredentialEgressModePinnedProxy,
								OutboundProxyID: lo.ToPtr(proxyCap1.ID),
							},
						},
					},
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-excess-2"}},
			},
		},
	}
	rawCreateExcess, err := json.Marshal(createExcessData)
	require.NoError(t, err)

	err = service.Restore(ctx, rawCreateExcess, RestoreOptions{
		IncludeChannels:         true,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
	})
	require.ErrorContains(t, err, "allows at most 1 credentials")

	countAfterCreateFail, err := client.Channel.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, countAfterCreateFail)

	// 2. Overwriting existing-bound-channel with 2 bound keys (> max_credentials=1) rolls back to original settings.
	overwriteExcessData := BackupData{
		Version: BackupVersion,
		Channels: []*BackupChannel{
			{
				Channel: ent.Channel{
					Name:             "existing-bound-channel",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
					Settings: &objects.ChannelSettings{
						CredentialPolicies: []objects.ChannelCredentialPolicy{
							{
								ID:              objects.ChannelAPIKeyFingerprint("sk-new-1"),
								EgressMode:      objects.CredentialEgressModePinnedProxy,
								OutboundProxyID: lo.ToPtr(proxyCap1.ID),
							},
							{
								ID:              objects.ChannelAPIKeyFingerprint("sk-new-2"),
								EgressMode:      objects.CredentialEgressModePinnedProxy,
								OutboundProxyID: lo.ToPtr(proxyCap1.ID),
							},
						},
					},
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-new-1", "sk-new-2"}},
			},
		},
	}
	rawOverwriteExcess, err := json.Marshal(overwriteExcessData)
	require.NoError(t, err)

	err = service.Restore(ctx, rawOverwriteExcess, RestoreOptions{
		IncludeChannels:         true,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
	})
	require.ErrorContains(t, err, "allows at most 1 credentials")

	reloadedExisting, err := client.Channel.Get(ctx, existingCh.ID)
	require.NoError(t, err)
	require.Equal(t, []string{existingKey}, reloadedExisting.Credentials.APIKeys)
	require.Len(t, reloadedExisting.Settings.CredentialPolicies, 1)
	require.Equal(t, "existing-pin", reloadedExisting.Settings.CredentialPolicies[0].Name)
}

func TestBackupService_RestoreCredentialPolicy_ValidAndLegacyNoPolicySucceed(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	proxySvc := biz.NewOutboundProxyService(biz.OutboundProxyServiceParams{Ent: client})
	proxyCap2, err := proxySvc.Create(ctx, biz.OutboundProxyCreateInput{
		Name:           "restore-valid-proxy",
		Kind:           biz.OutboundProxyKindStatic,
		ProxyURL:       "http://proxy.example.com:8080",
		Status:         biz.OutboundProxyStatusEnabled,
		MaxCredentials: lo.ToPtr(2),
	})
	require.NoError(t, err)

	policyKey := "sk-managed-valid"
	data := BackupData{
		Version: BackupVersion,
		Channels: []*BackupChannel{
			{
				Channel: ent.Channel{
					Name:             "legacy-no-policy-channel",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
					Settings: &objects.ChannelSettings{
						ExtraModelPrefix: "legacy/",
					},
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-legacy-only"}},
			},
			{
				Channel: ent.Channel{
					Name:             "managed-policy-channel",
					Type:             channel.TypeOpenai,
					BaseURL:          "https://api.openai.com/v1",
					Status:           channel.StatusEnabled,
					SupportedModels:  []string{"gpt-4o"},
					DefaultTestModel: "gpt-4o",
					Settings: &objects.ChannelSettings{
						CredentialPolicies: []objects.ChannelCredentialPolicy{
							{
								ID:               objects.ChannelAPIKeyFingerprint(policyKey),
								Name:             "restored-pinned",
								Priority:         10,
								Weight:           2,
								EgressMode:       objects.CredentialEgressModePinnedProxy,
								OutboundProxyID:  lo.ToPtr(proxyCap2.ID),
								EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
							},
						},
					},
				},
				Credentials: objects.ChannelCredentials{APIKeys: []string{policyKey}},
			},
		},
	}

	raw, err := json.Marshal(data)
	require.NoError(t, err)

	err = service.Restore(ctx, raw, RestoreOptions{
		IncludeChannels:         true,
		ChannelConflictStrategy: ConflictStrategyOverwrite,
	})
	require.NoError(t, err)

	legacyCh, err := client.Channel.Query().Where(channel.Name("legacy-no-policy-channel")).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "legacy/", legacyCh.Settings.ExtraModelPrefix)
	require.Empty(t, legacyCh.Settings.CredentialPolicies)

	managedCh, err := client.Channel.Query().Where(channel.Name("managed-policy-channel")).Only(ctx)
	require.NoError(t, err)
	require.Len(t, managedCh.Settings.CredentialPolicies, 1)
	require.Equal(t, "restored-pinned", managedCh.Settings.CredentialPolicies[0].Name)
	require.Equal(t, lo.ToPtr(proxyCap2.ID), managedCh.Settings.CredentialPolicies[0].OutboundProxyID)
}

func TestRestoreCredentialSourceRemapsChannelIDs(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()
	unrelated := createBackupTestChannel(t, client, ctx, "unrelated", channel.TypeOpenai)
	backupData := BackupData{
		Version: BackupVersion,
		Channels: []*BackupChannel{
			{Channel: ent.Channel{
				ID: unrelated.ID + 100, Name: "dependent", Type: channel.TypeOpenaiResponses, Status: channel.StatusEnabled,
				SupportedModels: []string{"m"}, DefaultTestModel: "m", Settings: &objects.ChannelSettings{CredentialSourceChannelID: &unrelated.ID},
			}},
			{Channel: ent.Channel{
				ID: unrelated.ID, Name: "holder", Type: channel.TypeOpenai, Status: channel.StatusDisabled,
				SupportedModels: []string{"m"}, DefaultTestModel: "m",
			}, Credentials: objects.ChannelCredentials{APIKey: "synthetic"}},
		},
	}
	raw, err := json.Marshal(backupData)
	require.NoError(t, err)
	require.NoError(t, service.Restore(ctx, raw, RestoreOptions{IncludeChannels: true, ChannelConflictStrategy: ConflictStrategyOverwrite}))
	holder := client.Channel.Query().Where(channel.Name("holder")).OnlyX(ctx)
	dependent := client.Channel.Query().Where(channel.Name("dependent")).OnlyX(ctx)
	require.NotEqual(t, unrelated.ID, holder.ID)
	require.Equal(t, &holder.ID, dependent.Settings.CredentialSourceChannelID)
	require.Empty(t, dependent.Credentials.GetAllAPIKeys())
}
