//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFetchKiroUpstreamModelsSendsProfileArnAndParsesIDs(t *testing.T) {
	account := &Account{
		ID:       801,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "kiro-access-token",
			"auth_method":  "idc",
			"provider":     "BuilderId",
			"region":       "us-east-1",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/ListAvailableModels", r.URL.Path)
		require.Equal(t, "AI_EDITOR", r.URL.Query().Get("origin"))
		require.Equal(t, kiroBuilderIDProfileARN, r.URL.Query().Get("profileArn"))
		require.Contains(t, r.Header.Get("User-Agent"), "KiroIDE-1.0.437-")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defaultModel": map[string]any{"modelId": "auto"},
			"models": []map[string]any{
				{"modelId": "claude-sonnet-4.6"},
				{"modelId": "claude-opus-4.6"},
				{"modelId": "auto"},
			},
		})
	}))
	defer server.Close()
	setKiroUsageTestEndpoint(t, server.URL)

	svc := &AccountTestService{}
	models, err := svc.fetchKiroUpstreamModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"auto", "claude-sonnet-4.6", "claude-opus-4.6"}, models)
}

func TestSyncKiroUpstreamModelsStoresTokenLimits(t *testing.T) {
	account := &Account{
		ID:       801,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "kiro-access-token",
			"auth_method":  "idc",
			"provider":     "BuilderId",
			"region":       "us-east-1",
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/ListAvailableModels", r.URL.Path)
		require.Contains(t, r.Header.Get("User-Agent"), "KiroIDE-1.0.437-")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"modelId": "claude-sonnet-4.6",
					"tokenLimits": map[string]any{
						"maxInputTokens":  1_000_000,
						"maxOutputTokens": 64_000,
					},
				},
				{
					"modelId": "gpt-5.6-sol",
					"tokenLimits": map[string]any{
						"maxInputTokens":  922_000,
						"maxOutputTokens": 128_000,
					},
				},
			},
		})
	}))
	defer server.Close()
	setKiroUsageTestEndpoint(t, server.URL)

	repo := &upstreamModelMetadataRepoStub{}
	svc := &AccountTestService{accountRepo: repo}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, catalog.Models, "claude-sonnet-4-6")
	require.Contains(t, catalog.Models, "claude-sonnet-4-6-thinking")
	require.Equal(t, int64(1_000_000), catalog.Metadata["claude-sonnet-4-6"].ContextWindow)
	require.Equal(t, int64(64_000), catalog.Metadata["claude-sonnet-4-6-thinking"].MaxOutputTokens)
	require.Equal(t, int64(922_000), catalog.Metadata["gpt-5.6-sol"].ContextWindow)
	require.Equal(t, int64(128_000), catalog.Metadata["gpt-5.6-sol"].MaxOutputTokens)

	snapshot, ok := repo.updates[UpstreamModelMetadataExtraKey].(UpstreamModelMetadataSnapshot)
	require.True(t, ok)
	require.Equal(t, "kiro", snapshot.Source)
	require.Equal(t, int64(1_000_000), snapshot.Models["claude-sonnet-4.6"].ContextWindow)
	require.Equal(t, int64(922_000), account.GetUpstreamModelMetadataSnapshot().Models["gpt-5.6-sol"].ContextWindow)
	require.Equal(t, int64(1_000_000), kiroStoredMaxInputTokens(account, "claude-sonnet-4-6-thinking"))
	require.Zero(t, kiroStoredMaxInputTokens(account, "claude-opus-4-6"))
}
