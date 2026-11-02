package objects

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectProfileModelIDsJSONRoundTripAndClonePreserveNilAndEmpty(t *testing.T) {
	tests := []struct {
		name       string
		json       string
		wantNil    bool
		wantValues []string
	}{
		{
			name:    "missing is nil",
			json:    `{"name":"legacy"}`,
			wantNil: true,
		},
		{
			name:    "null is nil",
			json:    `{"name":"legacy","modelIDs":null}`,
			wantNil: true,
		},
		{
			name:       "empty array is non nil",
			json:       `{"name":"deny","modelIDs":[]}`,
			wantValues: []string{},
		},
		{
			name:       "values are preserved",
			json:       `{"name":"allow","modelIDs":["gpt-4"]}`,
			wantValues: []string{"gpt-4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var profile ProjectProfile
			require.NoError(t, json.Unmarshal([]byte(tt.json), &profile))
			assertProjectProfileModelIDs(t, &profile, tt.wantNil, tt.wantValues)

			encoded, err := json.Marshal(profile)
			require.NoError(t, err)
			var roundTrip ProjectProfile
			require.NoError(t, json.Unmarshal(encoded, &roundTrip))
			assertProjectProfileModelIDs(t, &roundTrip, tt.wantNil, tt.wantValues)

			clone := profile
			if profile.ModelIDs != nil {
				modelIDs := slices.Clone(*profile.ModelIDs)
				clone.ModelIDs = &modelIDs
			}
			assertProjectProfileModelIDs(t, &clone, tt.wantNil, tt.wantValues)
		})
	}
}

func assertProjectProfileModelIDs(t *testing.T, profile *ProjectProfile, wantNil bool, wantValues []string) {
	t.Helper()
	if wantNil {
		require.Nil(t, profile.ModelIDs)
		return
	}

	require.NotNil(t, profile.ModelIDs)
	require.Equal(t, wantValues, *profile.ModelIDs)
}
