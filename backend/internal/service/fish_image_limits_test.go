package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFishMaxQualityFromAccountExtra(t *testing.T) {
	native := &Account{
		Platform:    PlatformFish,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{fishCredentialToken: "tok"},
		Extra:       map[string]any{fishMaxQualityExtraKey: "medium"},
	}
	require.Equal(t, "medium", native.FishMaxQuality())

	relay := &Account{
		Platform:    PlatformFish,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{fishCredentialToken: "tok", fishCredentialRelayBaseURL: "https://api.openai.com"},
		Extra:       native.Extra,
	}
	require.Equal(t, "", relay.FishMaxQuality())
	require.Equal(t, "", (&Account{Platform: PlatformFish, Type: AccountTypeAPIKey}).FishMaxQuality())

	invalid := &Account{
		Platform:    PlatformFish,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{fishCredentialToken: "tok"},
		Extra:       map[string]any{fishMaxQualityExtraKey: "ultra"},
	}
	require.Equal(t, "", invalid.FishMaxQuality())
}

func TestValidateFishMaxQualityFromExtra(t *testing.T) {
	require.NoError(t, ValidateFishMaxQualityFromExtra(nil))

	extra := map[string]any{fishMaxQualityExtraKey: " HIGH "}
	require.NoError(t, ValidateFishMaxQualityFromExtra(extra))
	require.Equal(t, "high", extra[fishMaxQualityExtraKey])

	require.NoError(t, ValidateFishMaxQualityFromExtra(map[string]any{fishMaxQualityExtraKey: ""}))
	require.NoError(t, ValidateFishMaxQualityFromExtra(map[string]any{}))

	highest := map[string]any{fishMaxQualityExtraKey: " MAX "}
	require.NoError(t, ValidateFishMaxQualityFromExtra(highest))
	require.Equal(t, "max", highest[fishMaxQualityExtraKey])
	require.NoError(t, ValidateFishMaxQualityFromExtra(map[string]any{fishMaxQualityExtraKey: "xhigh"}))

	require.Error(t, ValidateFishMaxQualityFromExtra(map[string]any{fishMaxQualityExtraKey: "ultra"}))
	require.Error(t, ValidateFishMaxQualityFromExtra(map[string]any{fishMaxQualityExtraKey: 3}))
}

func TestFishCapQuality(t *testing.T) {
	require.Equal(t, "medium", fishCapQuality("high", "medium"))
	require.Equal(t, "low", fishCapQuality("low", "high"))
	require.Equal(t, "high", fishCapQuality("high", ""))
	require.Equal(t, "medium", fishCapQuality("medium", "nope"))
	require.Equal(t, "xhigh", fishCapQuality("max", "xhigh"))
	require.Equal(t, "xhigh", fishCapQuality("xhigh", "max"))
}
