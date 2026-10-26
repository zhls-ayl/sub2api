package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// When no other account can serve the model either, the client still gets the
// fixed model-permission 403 instead of a generic upstream error.
func TestOpenAIExcelBPSModelNotAllowedFailoverExhausted_Returns403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:        http.StatusForbidden,
		ResponseBody:      []byte(`{"error":{"message":"must-not-leak"}}`),
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.ExcelBPSModelNotAllowedReason,
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusForbidden,
		ClientMessage:     "This model is not available on the account's Excel BPS endpoint",
	}, false)

	require.Equal(t, http.StatusForbidden, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	errBody, ok := envelope["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_request_error", errBody["type"])
	require.Equal(t, string(service.ExcelBPSModelNotAllowedReason), errBody["code"])
	require.Equal(t, "This model is not available on the account's Excel BPS endpoint", errBody["message"])
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

func exhaustExcelBPSFailover(t *testing.T, failoverErr *service.UpstreamFailoverError, anthropic bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	if anthropic {
		(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, failoverErr, false)
	} else {
		(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)
	}
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	errBody, ok := envelope["error"].(map[string]any)
	require.True(t, ok, rec.Body.String())
	return rec, errBody
}

// Exhausted BPS failovers keep their fixed BPS text: a BPS account rejection
// is a credential failure, but must not surface the Grok credential message.
func TestOpenAIExcelBPSFailoverExhausted_FixedMessages(t *testing.T) {
	cases := []struct {
		name       string
		failover   *service.UpstreamFailoverError
		wantStatus int
		wantType   string
		wantCode   string
	}{
		{name: "account rejected", failover: &service.UpstreamFailoverError{
			StatusCode: http.StatusForbidden, Stage: service.GatewayFailureStageAccountAuth, Scope: service.GatewayFailureScopeAccount,
			Reason: service.ExcelBPSAccountRejectedReason, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Excel BPS rejected the selected account",
		}, wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantCode: string(service.ExcelBPSAccountRejectedReason)},
		{name: "route unavailable", failover: &service.UpstreamFailoverError{
			StatusCode: http.StatusBadGateway, Scope: service.GatewayFailureScopeAccount,
			Reason: service.ExcelOAuthRouteUnavailableReason, ClientStatusCode: http.StatusBadGateway, ClientMessage: "an enabled Excel BPS route is required",
		}, wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantCode: string(service.ExcelOAuthRouteUnavailableReason)},
		{name: "inbound unsupported", failover: &service.UpstreamFailoverError{
			StatusCode: http.StatusBadGateway, Scope: service.GatewayFailureScopeAccount,
			Reason: service.BPSUnsupportedInboundReason, ClientStatusCode: http.StatusBadGateway, ClientMessage: "BPS OAuth accounts only serve the Responses API",
		}, wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantCode: string(service.BPSUnsupportedInboundReason)},
		{name: "upstream error", failover: &service.UpstreamFailoverError{
			StatusCode: 529, Scope: service.GatewayFailureScopeAccount,
			Reason: service.ExcelBPSUpstreamErrorReason, ClientStatusCode: http.StatusServiceUnavailable, ClientMessage: "Excel BPS upstream is temporarily unavailable, please retry later",
		}, wantStatus: http.StatusServiceUnavailable, wantType: "upstream_error", wantCode: string(service.ExcelBPSUpstreamErrorReason)},
		{name: "image unsupported", failover: &service.UpstreamFailoverError{
			StatusCode: http.StatusBadRequest, Scope: service.GatewayFailureScopeRequest,
			Reason: service.ExcelBPSImageUnsupportedReason, ClientStatusCode: http.StatusBadRequest, ClientMessage: "basispoints inline image exceeds the 20 MiB limit",
		}, wantStatus: http.StatusBadRequest, wantType: "invalid_request_error", wantCode: "basispoints_request_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.failover.ResponseBody = []byte(`{"error":{"message":"must-not-leak"}}`)
			rec, errBody := exhaustExcelBPSFailover(t, tc.failover, false)

			require.Equal(t, tc.wantStatus, rec.Code)
			require.Equal(t, tc.wantType, errBody["type"])
			require.Equal(t, tc.wantCode, errBody["code"])
			require.Equal(t, tc.failover.ClientMessage, errBody["message"])
			require.NotContains(t, rec.Body.String(), "must-not-leak")
			require.NotContains(t, rec.Body.String(), service.GrokCredentialUnavailableClientMessage)
		})
	}
}

func TestOpenAIExcelBPSFailoverExhausted_AnthropicMessages(t *testing.T) {
	rec, errBody := exhaustExcelBPSFailover(t, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadGateway, Scope: service.GatewayFailureScopeAccount, ResponseBody: []byte("must-not-leak"),
		Reason: service.BPSUnsupportedInboundReason, ClientStatusCode: http.StatusBadGateway, ClientMessage: "BPS OAuth accounts only serve the Responses API",
	}, true)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "api_error", errBody["type"])
	require.Equal(t, "BPS OAuth accounts only serve the Responses API", errBody["message"])
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}
