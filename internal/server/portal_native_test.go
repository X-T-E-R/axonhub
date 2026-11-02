package server

import (
	"testing"

	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/stretchr/testify/require"
)

// Exercises portal authorization through the real SetupRoutes and native
// pipeline, including aliases whose configured model association has its own
// logical candidate name. Provider payloads and credentials stay synthetic.
func TestPortalNativeDecisionAuthorization(t *testing.T) {
	f := newNativeDecisionFixture(t)
	owner := f.db.User.Create().SetEmail("portal-native@example.test").SetPassword("synthetic").SetAccessMode(user.AccessModePortal).SaveX(f.ctx)
	models := []string{"direct", "jev-1.14.0", "openai", "gpt-6-luna"}
	profiles := &objects.ProjectProfiles{ActiveProfile: "native", Profiles: []objects.ProjectProfile{{Name: "native", ModelIDs: &models, ChannelIDs: []int{f.channels[0].ID, f.channels[2].ID}}}}
	group := f.db.Project.Create().SetName("Portal native group").SetProfiles(profiles).SaveX(f.ctx)
	f.db.UserProject.Create().SetUserID(owner.ID).SetProjectID(group.ID).SaveX(f.ctx)
	key := f.db.APIKey.Create().SetName("portal-native").SetKey("ah-portal-native-synthetic").SetType(apikey.TypePersonal).SetPortalManaged(true).SetUserID(owner.ID).SetProjectID(group.ID).SetScopes([]string{"read_channels", "write_requests"}).SaveX(f.ctx)
	for _, spec := range []struct{ path, body string }{{"/v1/systemone", nativeSystemBody("direct", "")}, {"/v1/decisions", nativeOpenAIBody("")}} {
		status, _, body := f.post(t, spec.path, spec.body, key.Key, nil)
		require.Equal(t, 200, status, string(body))
	}
	require.EqualValues(t, 2, f.calls.Load())
	// Shrink the authoritative group without invalidating any project/key cache.
	models = []string{"direct", "jev-1.14.0"}
	f.db.Project.UpdateOne(group).SetProfiles(profiles).SaveX(f.ctx)
	status, _, _ := f.post(t, "/v1/decisions", nativeOpenAIBody(""), key.Key, nil)
	require.NotEqual(t, 200, status)
	require.EqualValues(t, 2, f.calls.Load())
	// A logical entry is independently checked; allowing the public alias alone
	// cannot select a candidate whose RequestModel is outside the group.
	models = []string{"direct"}
	f.db.Project.UpdateOne(group).SetProfiles(profiles).SaveX(f.ctx)
	status, _, _ = f.post(t, "/v1/systemone", nativeSystemBody("direct", ""), key.Key, nil)
	require.NotEqual(t, 200, status)
	require.EqualValues(t, 2, f.calls.Load())
	// Restore models but remove the eligible channel: no route fallback escapes.
	models = []string{"direct", "jev-1.14.0"}
	profiles.Profiles[0].ChannelIDs = []int{f.channels[2].ID}
	f.db.Project.UpdateOne(group).SetProfiles(profiles).SaveX(f.ctx)
	status, _, _ = f.post(t, "/v1/systemone", nativeSystemBody("direct", ""), key.Key, nil)
	require.NotEqual(t, 200, status)
	require.EqualValues(t, 2, f.calls.Load())
}
