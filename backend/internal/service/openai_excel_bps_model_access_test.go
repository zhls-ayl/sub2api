package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const bpsAccessFixture = `{"allowed":true,"model_catalog":{"restricted_models":["gpt-6-astra"],"models":[{"id":"gpt-6-astra"},{"id":"gpt-7-new","label":"New model","efforts":[{"value":"none"},{"value":"medium"},{"value":"high"},{"value":"high"}]},{"id":"gpt-hidden","model_picker_enabled":false},{"id":"gpt-policy-hidden","policy":{"state":"disabled"}}]}}`

func TestExcelBPSAccessCatalogFiltersPermissionsAndKeepsNewModels(t *testing.T) {
	body, err := excelBPSAccessModelsBody([]byte(bpsAccessFixture))
	require.NoError(t, err)
	require.Contains(t, string(body), `"id":"gpt-7-new"`)
	require.Contains(t, string(body), `"display_name":"New model"`)
	require.Contains(t, string(body), `"reasoning_efforts":["medium","high"]`)
	for _, id := range []string{"gpt-6-astra", "gpt-hidden", "gpt-policy-hidden"} {
		require.NotContains(t, string(body), id)
	}
	denied, err := excelBPSAccessModelsBody([]byte(`{"allowed":false}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"object":"list","data":[]}`, string(denied))
	for _, raw := range []string{`{}`, `{"allowed":true}`, `{"allowed":true,"model_catalog":{"models":null}}`, `{"allowed":"true"}`} {
		_, err := excelBPSAccessModelsBody([]byte(raw))
		require.Error(t, err)
	}
}

// Discovery only backs "sync upstream models", an explicit admin action, so
// every call reaches BPS: nothing is cached, rejections never leak the body,
// and a cancelled caller gets its own context error.
func TestExcelBPSAccessFetchIsNotCached(t *testing.T) {
	var calls atomic.Int32
	reject := false
	upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, "/basispoints/api/responses/access", req.URL.Path)
		require.Equal(t, "true", req.URL.Query().Get("include_models"))
		require.Equal(t, "chatgpt", req.Header.Get("X-Basispoints-Auth-Mode"))
		require.Contains(t, req.Header.Get("Authorization"), "Bearer ")
		require.EqualValues(t, 300, accountID)
		if reject {
			response := ordinaryModelsUpstreamResponse(`{"error":"SYNTHETIC_SECRET"}`)
			response.StatusCode = 403
			return response, nil
		}
		return ordinaryModelsUpstreamResponse(bpsAccessFixture), nil
	}}
	svc := openAIClientToolsTestService(nil)
	svc.httpUpstream = upstream
	account := excelAccount()
	for range 2 {
		response, err := svc.FetchExcelBPSModelsList(context.Background(), account)
		require.NoError(t, err)
		require.Contains(t, string(response.Body), `"gpt-7-new"`)
	}
	require.EqualValues(t, 2, calls.Load(), "discovery is never cached")

	reject = true
	_, err := svc.FetchExcelBPSModelsList(context.Background(), account)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SYNTHETIC_SECRET")
	require.True(t, account.IsExcelBPSEnabled(), "catalog rejection must not disable business routes")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = svc.FetchExcelBPSModelsList(ctx, account)
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 3, calls.Load())
}

// Model lists, Codex manifests and the test picker of a BPS-only account come
// from its configuration: mapping targets (projected back to public names), or
// the default BPS models. Neither BPS nor native Codex is called.
func TestExcelBPSModelListsComeFromConfiguration(t *testing.T) {
	var bpsCalls, nativeCalls atomic.Int32
	svc := openAIClientToolsTestService(nil)
	svc.httpUpstream = excelBPSAccessOnlyUpstream(t, &bpsCalls)
	useNativeCodexManifestServer(t, &nativeCalls)

	account := excelAccount()
	list, err := svc.FetchOpenAIModelsList(context.Background(), account)
	require.NoError(t, err)
	for _, model := range ExcelBPSDefaultModels {
		require.Contains(t, string(list.Body), `"`+model+`"`)
	}
	require.NotContains(t, string(list.Body), "gpt-5.5")

	account.Credentials["model_mapping"] = map[string]any{"public-new": "gpt-7-new"}
	list, err = svc.FetchOpenAIModelsList(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, string(list.Body), `"gpt-7-new"`)
	require.NotContains(t, string(list.Body), "gpt-6-astra", "a configured restriction replaces the defaults")

	manifest, err := svc.FetchCodexModelsManifest(context.Background(), account, "", "")
	require.NoError(t, err)
	require.Contains(t, string(manifest.Body), `"gpt-7-new"`)
	require.Contains(t, string(manifest.Body), `"multi_agent_version":null`)
	notModified, err := svc.FetchCodexModelsManifest(context.Background(), account, "", manifest.ETag)
	require.NoError(t, err)
	require.True(t, notModified.NotModified)

	picker := &AccountTestService{openaiGatewayService: svc}
	models, err := picker.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	require.Equal(t, []string{"public-new"}, ids)

	require.Zero(t, bpsCalls.Load(), "listings never call BPS discovery")
	require.Zero(t, nativeCalls.Load(), "Excel OAuth credentials must never reach native Codex discovery")
}

type excelBPSShadowParentRepo struct {
	AccountRepository
	parent *Account
}

func (r excelBPSShadowParentRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.parent, nil
}

// Spark shadows never route through BPS, and BPS OAuth accounts cannot parent
// one: a malformed shadow pointing at a BPS-only parent must fail without
// sending the Excel token to ChatGPT or BPS.
func TestExcelBPSShadowOfExcelParentRefusesNativeCatalog(t *testing.T) {
	var bpsCalls, nativeCalls atomic.Int32
	svc := openAIClientToolsTestService(nil)
	svc.httpUpstream = excelBPSAccessOnlyUpstream(t, &bpsCalls)
	useNativeCodexManifestServer(t, &nativeCalls)
	parent := excelAccount()
	svc.accountRepo = excelBPSShadowParentRepo{parent: parent}
	parentID := parent.ID
	shadow := &Account{ID: 301, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 10,
		ParentAccountID: &parentID, Credentials: map[string]any{}, Extra: map[string]any{}}

	_, err := svc.FetchOpenAIModelsList(context.Background(), shadow)
	require.Error(t, err)
	require.Zero(t, bpsCalls.Load())
	require.Zero(t, nativeCalls.Load())
}

// excelBPSAccessOnlyUpstream serves the BPS access catalog. Native OAuth
// manifests do not go through httpUpstream, so any other path is a test bug.
func excelBPSAccessOnlyUpstream(t *testing.T, calls *atomic.Int32) HTTPUpstream {
	return &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		require.True(t, strings.HasPrefix(req.URL.Path, "/basispoints/"), "unexpected upstream path %s", req.URL.Path)
		calls.Add(1)
		return ordinaryModelsUpstreamResponse(bpsAccessFixture), nil
	}}
}

// useNativeCodexManifestServer points the ChatGPT Codex manifest endpoint at a
// local server so native OAuth discovery never leaves the test process.
func useNativeCodexManifestServer(t *testing.T, calls *atomic.Int32) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-native","visibility":"list"}]}`))
	}))
	t.Cleanup(server.Close)
	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	t.Cleanup(func() { chatgptCodexModelsURL = original })
}
