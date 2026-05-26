package conf

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestRegistrationDefaultsAreClosedAndSelfServiceDisabled(t *testing.T) {
	v := viper.New()
	setDefaults(v)

	require.False(t, v.GetBool("registration.enabled"))
	require.False(t, v.GetBool("registration.self_service_enabled"))
	require.Empty(t, v.GetStringSlice("registration.default_project_scopes"))
	require.Empty(t, v.GetStringSlice("registration.self_service_preset_names"))
	require.False(t, v.GetBool("registration.allow_request_details"))
}
