//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
