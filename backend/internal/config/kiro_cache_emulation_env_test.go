//go:build unit

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadKiroCacheEmulationRedisFromEnv guards the env reachability of
// gateway.kiro_cache_emulation_redis_enabled: without a registered default,
// GATEWAY_KIRO_CACHE_EMULATION_REDIS_ENABLED is dropped by viper's AllKeys()
// union and env-only deployments silently keep the in-process tracker.
func TestLoadKiroCacheEmulationRedisFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_KIRO_CACHE_EMULATION_REDIS_ENABLED", "true")

	cfg, err := Load()
	require.NoError(t, err)

	require.True(t, cfg.Gateway.KiroCacheEmulationRedisEnabled,
		"env-only deployments must be able to enable the shared Kiro cache emulation store")
}
