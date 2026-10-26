//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// A BPS-only account holds the same OpenAI OAuth credentials as a Codex OAuth
// account, so a 401 opens a token-refresh window instead of a permanent error.
func TestRateLimitService_HandleUpstreamError_BPSOAuth401SetsTempUnschedulable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	invalidator := &tokenCacheInvalidatorRecorder{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetTokenCacheInvalidator(invalidator)
	account := &Account{
		ID:       2890,
		Platform: PlatformOpenAI,
		Type:     AccountTypeBPSOAuth,
		Credentials: map[string]any{
			"access_token":  "expired-at",
			"refresh_token": "rt-2890",
		},
	}

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, 401, http.Header{}, []byte(`{"error":{"message":"Excel BPS authentication failed"}}`))

	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls, "BPS OAuth 401 must keep the account recoverable by token refresh")
	require.Equal(t, 1, repo.tempCalls)
	require.Len(t, invalidator.accounts, 1)
}
