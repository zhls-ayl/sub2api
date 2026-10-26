//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// excelBPSImageRequestBody carries an image that BPS cannot reference (only
// HTTPS URLs, file IDs and validated data URLs are allowed) but a native
// account may accept.
const excelBPSImageRequestBody = `{"model":"gpt-5.6-sol","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"http://images.example/photo.png"}]}]}`

func excelBPSImageFailoverAccount(id int64, accountType string) service.Account {
	account := service.Account{
		ID: id, Name: "account", Platform: service.PlatformOpenAI, Type: accountType,
		Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "token"},
	}
	if accountType == service.AccountTypeBPSOAuth {
		account.Credentials["client_id"] = openai.ExcelClientID
		account.Credentials["chatgpt_account_id"] = "workspace"
	}
	return account
}

func newExcelBPSImageFailoverHandler(t *testing.T, upstream service.HTTPUpstream, accounts ...service.Account) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(gatewayService, service.NewConcurrencyService(nil), billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, nil, cfg)
	handler.maxAccountSwitches = 10
	return handler
}

func newExcelBPSImageFailoverContext() (*gin.Context, *httptest.ResponseRecorder) {
	groupID := int64(3141)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(excelBPSImageRequestBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}, User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
	return c, rec
}

// In a mixed pool whose BPS accounts rank first (lower priority value), a BPS
// account is tried first; its image rejection keeps the rest of the request off
// BPS and a native account serves it.
func TestOpenAIGatewayHandlerResponses_ExcelBPSImageFailureMovesToNative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newAstraProCapturedUpstream(&http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(`{"id":"resp_native","model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	})
	bps1 := excelBPSImageFailoverAccount(1, service.AccountTypeBPSOAuth)
	bps2 := excelBPSImageFailoverAccount(2, service.AccountTypeBPSOAuth)
	native := excelBPSImageFailoverAccount(3, service.AccountTypeOAuth)
	native.Priority = 5
	handler := newExcelBPSImageFailoverHandler(t, upstream, bps1, bps2, native)
	// One switch only: retrying the second BPS account would exhaust it.
	handler.maxAccountSwitches = 1
	c, rec := newExcelBPSImageFailoverContext()

	handler.Responses(c)

	urls, accountIDs, _ := upstream.snapshot()
	require.Equal(t, []int64{3}, accountIDs, "only the native account reaches an upstream, and the second BPS account is skipped")
	require.Equal(t, astraProCodexURL, urls[0])
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Without a native account the client gets the protocol's explicit 400.
func TestOpenAIGatewayHandlerResponses_ExcelBPSImageFailureOnlyBPS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newAstraProCapturedUpstream()
	handler := newExcelBPSImageFailoverHandler(t, upstream,
		excelBPSImageFailoverAccount(1, service.AccountTypeBPSOAuth), excelBPSImageFailoverAccount(2, service.AccountTypeBPSOAuth))
	c, rec := newExcelBPSImageFailoverContext()

	handler.Responses(c)

	urls, _, _ := upstream.snapshot()
	require.Empty(t, urls)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "basispoints_request_invalid", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), "HTTPS image URL")
}
