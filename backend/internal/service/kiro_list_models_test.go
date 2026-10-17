//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestKiroListAvailableModelsPaginatesAndUsesProfileArn(t *testing.T) {
	account := &Account{
		ID:       7,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"api_region": "us-east-1",
		},
	}
	var requests []*http.Request
	do := func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req)
		body := `{"models":[{"modelId":"claude-opus-5.5"}],"nextToken":""}`
		if len(requests) == 1 {
			body = `{"models":[{"modelId":"claude-opus-5.5"},{"modelId":"gpt-5.6-sol"}],"nextToken":"page-2"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	ids, err := kiroListAvailableModelIDsWithDo(context.Background(), account, "token", do)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-5.5", "gpt-5.6-sol"}, ids)
	require.Len(t, requests, 2)

	first := requests[0].URL
	require.Equal(t, http.MethodGet, requests[0].Method)
	require.Equal(t, "q.us-east-1.amazonaws.com", first.Host)
	require.Equal(t, "/ListAvailableModels", first.Path)
	require.Equal(t, "AI_EDITOR", first.Query().Get("origin"))
	require.Equal(t, "50", first.Query().Get("maxResults"))
	require.Equal(t, kiroBuilderIDProfileARN, first.Query().Get("profileArn"))
	require.Empty(t, first.Query().Get("nextToken"))
	require.Equal(t, "Bearer token", requests[0].Header.Get("Authorization"))
	require.Equal(t, "application/json", requests[0].Header.Get("Accept"))
	require.Empty(t, requests[0].Header.Get("X-Amz-Target"))
	require.Equal(t, "page-2", requests[1].URL.Query().Get("nextToken"))
	require.Equal(t, []string{"claude-opus-5-5", "claude-opus-5-5-thinking", "gpt-5.6-sol"}, kiroSyncPublicModelNames(ids))
	_, metadata := kiroPublicCatalog(ids)
	require.Equal(t, "claude-opus-5.5", metadata["claude-opus-5-5"].ID)
	require.Equal(t, "claude-opus-5.5", metadata["claude-opus-5-5-thinking"].ID)
	require.Equal(t, "gpt-5.6-sol", metadata["gpt-5.6-sol"].ID)
}

func TestKiroListAvailableModelsRetriesForbiddenRegion(t *testing.T) {
	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"api_region": "us-east-1",
		},
	}
	var hosts []string
	do := func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if req.URL.Host == "q.us-east-1.amazonaws.com" {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"forbidden"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"models":[{"modelId":"claude-sonnet-5"}]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	ids, err := kiroListAvailableModelIDsWithDo(context.Background(), account, "token", do)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-5"}, ids)
	require.Equal(t, []string{"q.us-east-1.amazonaws.com", "q.eu-central-1.amazonaws.com"}, hosts)
}

func TestKiroListAvailableModelsRetriesForbiddenRegionFromEU(t *testing.T) {
	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"api_region": "eu-central-1",
		},
	}
	var hosts []string
	do := func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if req.URL.Host == "q.eu-central-1.amazonaws.com" {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"message":"forbidden"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"models":[{"modelId":"claude-sonnet-4.6"}]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	ids, err := kiroListAvailableModelIDsWithDo(context.Background(), account, "token", do)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-4.6"}, ids)
	require.Equal(t, []string{"q.eu-central-1.amazonaws.com", "q.us-east-1.amazonaws.com"}, hosts)
}

func TestKiroListAvailableModelsOAuthUsesPinnedUsageHeaders(t *testing.T) {
	oauth := &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}
	apiKey := &Account{Platform: PlatformKiro, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "ksk_test"}}
	var oauthReq, apiKeyReq *http.Request
	do := func(dst **http.Request) kiroModelsDoFunc {
		return func(req *http.Request) (*http.Response, error) {
			*dst = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"models":[{"modelId":"claude-sonnet-4.6"}]}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
	}

	_, err := kiroListAvailableModelIDsWithDo(context.Background(), oauth, "token", do(&oauthReq))
	require.NoError(t, err)
	require.Contains(t, oauthReq.Header.Get("User-Agent"), "api/codewhispererstreaming#1.0.34")
	require.Contains(t, oauthReq.Header.Get("User-Agent"), "aws-sdk-js/1.0.34")
	require.Contains(t, oauthReq.Header.Get("X-Amz-User-Agent"), "aws-sdk-js/1.0.34")
	require.Equal(t, "vibe", oauthReq.Header.Get("x-amzn-kiro-agent-mode"))
	require.Equal(t, "attempt=1; max=1", oauthReq.Header.Get("Amz-Sdk-Request"))
	oauthUA := oauthReq.Header.Get("User-Agent")
	require.NotContains(t, oauthUA, "os/darwin")
	require.True(t, strings.Contains(oauthUA, "os/macos") || strings.Contains(oauthUA, "os/win32"))

	_, err = kiroListAvailableModelIDsWithDo(context.Background(), apiKey, "ksk_test", do(&apiKeyReq))
	require.NoError(t, err)
	require.Empty(t, apiKeyReq.Header.Get("x-amzn-kiro-agent-mode"))
	require.Equal(t, "attempt=1; max=3", apiKeyReq.Header.Get("Amz-Sdk-Request"))
}

func TestKiroListAvailableModelsAPIKeyOmitsProfileArn(t *testing.T) {
	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "ksk_test",
		},
	}
	do := func(req *http.Request) (*http.Response, error) {
		require.Empty(t, req.URL.Query().Get("profileArn"))
		require.Equal(t, []string{"API_KEY"}, req.Header["TokenType"])
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"models":[{"modelId":"claude-opus-5"}]}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	ids, err := kiroListAvailableModelIDsWithDo(context.Background(), account, "ksk_test", do)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-5"}, ids)
	require.Equal(t, []string{"claude-opus-5", "claude-opus-5-thinking"}, kiroSyncPublicModelNames(ids))
}

func TestKiroListAvailableModelsParsesTokenLimits(t *testing.T) {
	account := &Account{Platform: PlatformKiro, Type: AccountTypeAPIKey}
	do := func(req *http.Request) (*http.Response, error) {
		body := `{
			"models":[
				{"modelId":"claude-opus-4.6","tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":64000}},
				{"modelId":"claude-haiku-4.5","tokenLimits":{"maxInputTokens":200000,"maxOutputTokens":64000}},
				{"modelId":"deepseek-3.2","tokenLimits":{"maxInputTokens":164000,"maxOutputTokens":-1}},
				{"modelId":"claude-sonnet-4.5"}
			]
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	listed, err := kiroListAvailableModelsWithDo(context.Background(), account, "ksk_test", do)
	require.NoError(t, err)
	require.Equal(t, []kiroListedModel{
		{ID: "claude-opus-4.6", MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000},
		{ID: "claude-haiku-4.5", MaxInputTokens: 200_000, MaxOutputTokens: 64_000},
		{ID: "deepseek-3.2", MaxInputTokens: 164_000},
		{ID: "claude-sonnet-4.5"},
	}, listed)

	names, metadata := kiroPublicCatalogFromListed(listed)
	require.Contains(t, names, "claude-opus-4-6")
	require.Contains(t, names, "claude-opus-4-6-thinking")
	require.Contains(t, names, "claude-haiku-4-5-20251001")
	require.Equal(t, int64(1_000_000), metadata["claude-opus-4-6"].ContextWindow)
	require.Equal(t, int64(1_000_000), metadata["claude-opus-4-6"].MaxContextWindow)
	require.Equal(t, int64(64_000), metadata["claude-opus-4-6"].MaxOutputTokens)
	require.Equal(t, int64(1_000_000), metadata["claude-opus-4-6-thinking"].ContextWindow)
	require.Equal(t, int64(1_000_000), metadata["claude-opus-4.6"].ContextWindow)
	require.Equal(t, int64(200_000), metadata["claude-haiku-4-5-20251001-thinking"].ContextWindow)
	require.Equal(t, int64(164_000), metadata["deepseek-3.2"].ContextWindow)
	require.Zero(t, metadata["deepseek-3.2"].MaxOutputTokens)
	require.Zero(t, metadata["claude-sonnet-4-5"].ContextWindow)

	account.ID = 41
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source: "kiro",
		Models: map[string]UpstreamModelMetadata{
			"claude-sonnet-4-5": {ID: "claude-sonnet-4.5", ContextWindow: 200_000},
		},
	})
	repo := &upstreamModelMetadataRepoStub{}
	svc := &AccountTestService{accountRepo: repo}
	catalog, err := svc.kiroCatalogFromListed(context.Background(), account, listed)
	require.NoError(t, err)
	require.Equal(t, names, catalog.Models)
	require.Equal(t, account.ID, repo.accountID)
	snapshot, ok := repo.updates[UpstreamModelMetadataExtraKey].(UpstreamModelMetadataSnapshot)
	require.True(t, ok)
	require.Equal(t, "kiro", snapshot.Source)
	require.Equal(t, int64(1_000_000), snapshot.Models["claude-opus-4-6"].ContextWindow)
	require.NotContains(t, snapshot.Models, "claude-sonnet-4-5")
	stored, found := account.GetUpstreamModelMetadata("claude-opus-4-6-thinking")
	require.True(t, found)
	require.Equal(t, int64(1_000_000), stored.ContextWindow)
	require.Equal(t, int64(1_000_000), kiroStoredMaxInputTokens(account, "claude-opus-4.6"))
	require.Equal(t, int64(1_000_000), kiroStoredMaxInputTokens(account, "claude-opus-4-6-thinking"))
	require.Equal(t, int64(200_000), kiroStoredMaxInputTokens(account, "claude-haiku-4-5-20251001"))
	require.Zero(t, kiroStoredMaxInputTokens(account, "gpt-5.6-sol"))
	require.Zero(t, kiroStoredMaxInputTokens(account, "claude-sonnet-4.5"))

	repo.updates = nil
	require.NoError(t, svc.persistKiroModelTokenLimits(context.Background(), account, map[string]UpstreamModelMetadata{
		"claude-sonnet-4-5": {ID: "claude-sonnet-4.5"},
	}))
	require.Nil(t, repo.updates)
	kept, found := account.GetUpstreamModelMetadata("claude-opus-4-6")
	require.True(t, found)
	require.Equal(t, int64(1_000_000), kept.ContextWindow)
}

func TestKiroListAvailableModelsStopsAfterTenPages(t *testing.T) {
	account := &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}
	calls := 0
	do := func(req *http.Request) (*http.Response, error) {
		calls++
		body := `{"models":[{"modelId":"claude-opus-5"}],"nextToken":"more"}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}

	ids, err := kiroListAvailableModelIDsWithDo(context.Background(), account, "token", do)
	require.NoError(t, err)
	require.Equal(t, kiroListModelsMaxPages, calls)
	require.Equal(t, []string{"claude-opus-5"}, ids)
}

func TestBuildKiroRelayModelsRequestUsesBaseURL(t *testing.T) {
	svc := &AccountTestService{cfg: &config.Config{Security: config.SecurityConfig{
		URLAllowlist: config.URLAllowlistConfig{Enabled: false},
	}}}
	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "relay-key",
			"base_url": "https://relay.example.com/v1",
		},
	}

	req, err := svc.buildKiroRelayModelsRequest(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com/v1/models", req.URL.String())
	require.Equal(t, "relay-key", req.Header.Get("x-api-key"))
	require.Equal(t, "2023-06-01", req.Header.Get("anthropic-version"))
}
