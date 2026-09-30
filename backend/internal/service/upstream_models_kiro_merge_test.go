//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKiroOAuthModelSyncPreservesProfileFallbackAndPublicAliases(t *testing.T) {
	for _, syncCatalog := range []bool{false, true} {
		name := "fetch"
		if syncCatalog {
			name = "catalog"
		}
		t.Run(name, func(t *testing.T) {
			account := &Account{
				ID: 802, Platform: PlatformKiro, Type: AccountTypeOAuth,
				Credentials: map[string]any{
					"access_token": "kiro-test-token", "auth_method": "idc",
					"provider": "BuilderId", "region": "eu-central-1",
				},
			}
			primaryHits, fallbackHits := 0, 0
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryHits++
				http.Error(w, `{"message":"Invalid token"}`, http.StatusForbidden)
			}))
			defer primary.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackHits++
				require.Equal(t, "/ListAvailableModels", r.URL.Path)
				require.Equal(t, kiroBuilderIDProfileARN, r.URL.Query().Get("profileArn"))
				require.Equal(t, "Bearer kiro-test-token", r.Header.Get("Authorization"))
				require.Contains(t, r.Header.Get("User-Agent"), "KiroIDE-1.0.437-")
				require.Equal(t, "vibe", r.Header.Get("x-amzn-kiro-agent-mode"))
				_, _ = io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.6"},{"modelId":"gpt-5.6-sol"}]}`)
			}))
			defer fallback.Close()
			previous := resolveKiroRuntimeEndpoint
			resolveKiroRuntimeEndpoint = func(region string) string {
				if region == kiroUsagePrimaryEURegion {
					return primary.URL
				}
				return fallback.URL
			}
			t.Cleanup(func() { resolveKiroRuntimeEndpoint = previous })

			svc := &AccountTestService{}
			want := []string{"claude-sonnet-4-6", "claude-sonnet-4-6-thinking", "gpt-5.6-sol"}
			if syncCatalog {
				catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
				require.NoError(t, err)
				require.Equal(t, want, catalog.Models)
				require.Equal(t, "claude-sonnet-4.6", catalog.Metadata["claude-sonnet-4-6"].ID)
				require.Equal(t, "claude-sonnet-4.6", catalog.Metadata["claude-sonnet-4-6-thinking"].ID)
			} else {
				models, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
				require.NoError(t, err)
				require.Equal(t, want, models)
			}
			require.Equal(t, 1, primaryHits)
			require.Equal(t, 1, fallbackHits)
		})
	}
}

func TestFetchKiroRelayModelsUsesRelayCatalog(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"relay-custom-model"}]}`)),
	}}
	svc := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		Platform: PlatformKiro, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://relay.example.com/v1", "api_key": "relay-key"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"relay-custom-model"}, models)
	require.Equal(t, "https://relay.example.com/v1/models", upstream.lastReq.URL.String())
	require.Equal(t, "relay-key", upstream.lastReq.Header.Get("x-api-key"))
}
