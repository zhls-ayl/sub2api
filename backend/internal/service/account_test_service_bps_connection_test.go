package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newBPSAccountTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/300/test", nil).WithContext(context.Background())
	return c, rec
}

// The default account test for a BPS-enabled account must take the gateway
// BPS route instead of the hard-coded native Codex probe.
func TestAccountTestServiceRoutesBPSAccountThroughBPS(t *testing.T) {
	wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}}
	svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream)}
	c, rec := newBPSAccountTestContext()

	err := svc.testOpenAIAccountConnection(c, excelAccount(), "gpt-5.6-sol", "", "")

	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, HTTPUpstreamProfileExcelBPS, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
	require.Contains(t, rec.Body.String(), "test_complete")
	require.NotContains(t, rec.Body.String(), `"type":"error"`)
}

// Excel OAuth tokens only work on BPS; for a model outside the account's
// model restriction the test must fail closed instead of sending the Excel
// token to ChatGPT Codex.
func TestAccountTestServiceExcelOAuthRequiresBPSRoute(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	gateway := openAIClientToolsTestService(upstream)
	svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: upstream}
	account := excelAccount()
	account.Credentials["client_id"] = openai.ExcelClientID
	account.Credentials["model_mapping"] = map[string]any{"gpt-6-astra": "gpt-6-astra"}
	c, rec := newBPSAccountTestContext()

	_ = svc.testOpenAIAccountConnection(c, account, "gpt-5.6-sol", "", "")

	require.Empty(t, upstream.requests)
	require.Contains(t, rec.Body.String(), errExcelOAuthRouteUnavailable.Error())
}

// The BPS tool probe exists only for BPS OAuth accounts.
func TestAccountTestServiceBPSToolsModeRequiresBPSOAuth(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream), httpUpstream: upstream}
	c, rec := newBPSAccountTestContext()
	codex := &Account{ID: 302, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "codex-token"}}

	_ = svc.testOpenAIAccountConnection(c, codex, "gpt-5.6-sol", "", AccountTestModeBPSTools)

	require.Empty(t, upstream.requests)
	require.Contains(t, rec.Body.String(), "BPS tool probe requires a BPS OAuth account")
}

// BPS-only accounts never fall through to the native image probe.
func TestAccountTestServiceBPSOAuthRejectsImageModels(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream), httpUpstream: upstream}
	c, rec := newBPSAccountTestContext()

	_ = svc.testOpenAIAccountConnection(c, excelAccount(), "gpt-image-2", "", "")

	require.Empty(t, upstream.requests)
	require.Contains(t, rec.Body.String(), "do not serve image models")
}

// A failing BPS test must show the BPS status and its own (redacted) message
// in the dialog instead of only basispoints_upstream_error.
func TestAccountTestServiceShowsBPSRejectionStatusAndMessage(t *testing.T) {
	body := `{"error":{"code":"token_invalidated","message":"Bearer test-token was rejected for this workspace"}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_bps_401"}}, Body: io.NopCloser(strings.NewReader(body))}}
	svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream)}
	c, rec := newBPSAccountTestContext()

	_ = svc.testOpenAIAccountConnection(c, excelAccount(), "gpt-5.6-sol", "", "")

	out := rec.Body.String()
	require.Len(t, upstream.requests, 1)
	require.Contains(t, out, `"type":"upstream_response"`)
	require.Contains(t, out, `"upstream_status":401`)
	require.Contains(t, out, `"request_id":"req_bps_401"`)
	require.Contains(t, out, `"upstream_error_code":"token_invalidated"`)
	require.Contains(t, out, `"upstream_error_message":`)
	require.Contains(t, out, "was rejected for this workspace")
	require.NotContains(t, out, "test-token", "the request token must never reach the test output")
	require.Contains(t, out, "(HTTP 401)")
}

func bpsTestSuccessResponse() *http.Response {
	wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
}

// bpsResponsesBodies returns the bodies sent to BPS Responses. The recorder
// keeps bodies only for requests that have one (discovery GETs have none).
func bpsResponsesBodies(upstream *httpUpstreamRecorder) [][]byte {
	var bodies [][]byte
	next := 0
	for _, req := range upstream.requests {
		if req.Body == nil {
			continue
		}
		if req.URL.String() == basispoints.ResponsesURL {
			bodies = append(bodies, upstream.bodies[next])
		}
		next++
	}
	return bodies
}

// The default test sends the public name like a user request, so the account
// mapping is applied exactly once.
func TestAccountTestServiceBPSMapsTestModelOnce(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: bpsTestSuccessResponse()}
	svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream)}
	account := excelAccount()
	account.Credentials["model_mapping"] = map[string]any{"my-sol": "gpt-5.6-sol", "gpt-5.6-sol": "gpt-5.6-terra"}
	c, rec := newBPSAccountTestContext()

	require.NoError(t, svc.testOpenAIAccountConnection(c, account, "my-sol", "", ""))

	bodies := bpsResponsesBodies(upstream)
	require.Len(t, bodies, 1)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(bodies[0], "model").String())
	require.Contains(t, rec.Body.String(), `"type":"test_start","model":"my-sol"`)
}

// Without a model the test uses the account's first configured model (here a
// default BPS model) instead of the native default, which BPS does not serve.
// Picking it never calls BPS discovery.
func TestAccountTestServiceBPSDefaultsToBPSModel(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{bpsTestSuccessResponse()}}
	svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream)}
	account := excelAccount()
	c, rec := newBPSAccountTestContext()

	require.NoError(t, svc.testOpenAIAccountConnection(c, account, "", "", ""))

	bodies := bpsResponsesBodies(upstream)
	require.Len(t, bodies, 1)
	require.Equal(t, ExcelBPSDefaultModels[0], gjson.GetBytes(bodies[0], "model").String())
	require.Contains(t, rec.Body.String(), "test_complete")
}

func TestAccountTestServiceBPSWithoutAnyModelFails(t *testing.T) {
	for _, mode := range []string{"", AccountTestModeBPSTools} {
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}}
		svc := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream)}
		account := excelAccount()
		// Only a wildcard: nothing listable to pick as the test model.
		account.Credentials["model_mapping"] = map[string]any{"gpt-*": "gpt-5.6-sol"}
		c, rec := newBPSAccountTestContext()

		_ = svc.testOpenAIAccountConnection(c, account, "", "", mode)

		require.Empty(t, bpsResponsesBodies(upstream), "no Responses request without a model")
		require.Contains(t, rec.Body.String(), errExcelBPSNoTestModel.Error())
	}
}
