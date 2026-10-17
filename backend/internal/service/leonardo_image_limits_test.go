package service

import (
	"testing"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/stretchr/testify/require"
)

func TestLeonardoImageLimitsFromAccountExtra(t *testing.T) {
	cookie := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Extra: map[string]any{
		"leonardo_max_quality":        "medium",
		"leonardo_max_short_edge":     float64(1024),
		"leonardo_max_upscale_factor": "1.5",
	}}
	require.Equal(t, leonardopkg.ImageLimits{MaxQuality: "MEDIUM", MaxShortEdge: 1024, MaxUpscaleFactor: 1.5}, cookie.LeonardoImageLimits())

	// 中转号、其它平台、未配置、无效值：不限。
	relay := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Extra: cookie.Extra}
	require.Equal(t, leonardopkg.ImageLimits{}, relay.LeonardoImageLimits())
	require.Equal(t, leonardopkg.ImageLimits{}, (&Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth}).LeonardoImageLimits())
	invalid := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Extra: map[string]any{
		"leonardo_max_quality":        "ultra",
		"leonardo_max_short_edge":     100,
		"leonardo_max_upscale_factor": 9,
	}}
	require.Equal(t, leonardopkg.ImageLimits{}, invalid.LeonardoImageLimits())
}

func TestValidateLeonardoImageLimitsFromExtra(t *testing.T) {
	require.NoError(t, ValidateLeonardoImageLimitsFromExtra(nil))

	extra := map[string]any{
		"leonardo_max_quality":        " HIGH ",
		"leonardo_max_short_edge":     "1024",
		"leonardo_max_upscale_factor": 2,
	}
	require.NoError(t, ValidateLeonardoImageLimitsFromExtra(extra))
	require.Equal(t, "high", extra["leonardo_max_quality"])
	require.Equal(t, 1024, extra["leonardo_max_short_edge"])
	require.Equal(t, 2.0, extra["leonardo_max_upscale_factor"])

	// 0 / 空表示不限或默认。
	require.NoError(t, ValidateLeonardoImageLimitsFromExtra(map[string]any{
		"leonardo_max_quality": "", "leonardo_max_short_edge": 0, "leonardo_max_upscale_factor": 0,
	}))

	for name, extra := range map[string]map[string]any{
		"unknown quality":    {"leonardo_max_quality": "ultra"},
		"quality not string": {"leonardo_max_quality": 3},
		"edge too small":     {"leonardo_max_short_edge": 512},
		"edge too large":     {"leonardo_max_short_edge": 4096},
		"edge not integer":   {"leonardo_max_short_edge": 1024.5},
		"edge not number":    {"leonardo_max_short_edge": "big"},
		"factor below 1":     {"leonardo_max_upscale_factor": 0.5},
		"factor above 4":     {"leonardo_max_upscale_factor": 5},
	} {
		require.Error(t, ValidateLeonardoImageLimitsFromExtra(extra), name)
	}
}
