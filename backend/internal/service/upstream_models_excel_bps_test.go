package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func excelBPSSyncTestService(status int, body string) (*AccountTestService, *int) {
	calls := 0
	gateway := openAIClientToolsTestService(nil)
	gateway.httpUpstream = &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		calls++
		if req.URL.Path != "/basispoints/api/responses/access" {
			return nil, errors.New("unexpected request " + req.URL.String())
		}
		response := ordinaryModelsUpstreamResponse(body)
		response.StatusCode = status
		return response, nil
	}}
	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig()}
	svc.SetOpenAIGatewayService(gateway)
	return svc, &calls
}

// A BPS-only account syncs from BPS model discovery, not native Codex, and
// sees the full catalog regardless of its configured model restriction. Each
// sync asks BPS again: discovery is not cached.
func TestFetchUpstreamSupportedModelsUsesExcelBPSDiscoveryForBPSOAuth(t *testing.T) {
	svc, calls := excelBPSSyncTestService(http.StatusOK, bpsAccessFixture)
	account := excelAccount()
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), account)

	require.NoError(t, err)
	require.Equal(t, []string{"gpt-7-new"}, models)
	require.Equal(t, 1, *calls)

	_, err = svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 2, *calls)
}

func TestSyncUpstreamModelCatalogReportsExcelBPSFailures(t *testing.T) {
	svc, _ := excelBPSSyncTestService(http.StatusForbidden, `{"error":"SYNTHETIC_SECRET"}`)

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), excelAccount())

	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
	require.Contains(t, syncErr.SafeMessage(), "HTTP 403")
	require.NotContains(t, syncErr.SafeMessage(), "SYNTHETIC_SECRET")

	free := excelAccount()
	free.Credentials["plan_type"] = "free"
	_, err = svc.SyncUpstreamModelCatalog(context.Background(), free)
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorConfiguration, syncErr.Kind)
}
