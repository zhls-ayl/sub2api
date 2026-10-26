package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func forwardExcelBPSWithStatus(t *testing.T, status int, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}}
	svc := openAIClientToolsTestService(upstream)
	request, err := buildExcelBPSAccountTestBody("gpt-5.6-sol", "hi")
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request))
	_, err = svc.Forward(context.Background(), c, excelAccount(), request)
	require.Len(t, upstream.requests, 1)
	return rec, err
}

// A BPS-only account has no other channel: a generic BPS 401/403 disqualifies
// the whole account, so the request moves on to another account without any
// BPS text reaching the client.
func TestExcelBPSAccountRejectionFailsOverToAnotherAccount(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		rec, err := forwardExcelBPSWithStatus(t, status, `{"error":{"code":"account_blocked","message":"private upstream diagnostic"}}`)

		var failover *UpstreamFailoverError
		require.True(t, errors.As(err, &failover), "status %d", status)
		require.Equal(t, ExcelBPSAccountRejectedReason, failover.Reason)
		require.Equal(t, status, failover.StatusCode)
		require.True(t, failover.ShouldRetryNextAccount())
		require.True(t, failover.IsCredentialFailure())
		require.Empty(t, failover.ResponseBody)
		require.NotContains(t, failover.ClientMessage, "private")
		require.False(t, rec.Body.Len() > 0, "leave the response to the handler")
	}
}

// A model-permission 403 only cools that (account, model): it is not an
// account rejection, but another account may still serve the model, so the
// request moves on without any BPS text reaching the client.
func TestExcelBPSModelPermission403FailsOverWithoutRejectingAccount(t *testing.T) {
	for _, code := range []string{"model_not_allowed", "basispoints_model_access_changed"} {
		gin.SetMode(gin.TestMode)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":"` + code + `","message":"private upstream diagnostic"}}`))}}
		svc := openAIClientToolsTestService(upstream)
		request, err := buildExcelBPSAccountTestBody("gpt-5.6-sol", "hi")
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request))

		_, err = svc.Forward(context.Background(), c, excelAccount(), request)

		var failover *UpstreamFailoverError
		require.True(t, errors.As(err, &failover), code)
		require.Equal(t, ExcelBPSModelNotAllowedReason, failover.Reason)
		require.True(t, failover.ShouldRetryNextAccount())
		require.False(t, failover.IsCredentialFailure())
		require.False(t, failover.ShouldReportAccountScheduleFailure(), "keep the shared scheduler score")
		require.Empty(t, failover.ResponseBody)
		require.NotContains(t, failover.ClientMessage, "private")
		require.Zero(t, rec.Body.Len(), "leave the response to the handler")
		require.True(t, svc.isExcelBPSModelCoolingDown(excelAccount(), "gpt-5.6-sol"))
		require.False(t, svc.isExcelBPSCoolingDown(excelAccount(), "gpt-5.6-sol"), "the account's other BPS models stay eligible")
	}
}

// excelBPSAccountStateRepo records the account-level writes of the shared
// upstream error policy.
type excelBPSAccountStateRepo struct {
	AccountRepository
	errors            map[int64]string
	overloaded        map[int64]time.Time
	modelLimits       map[int64][]string
	tempUnschedulable map[int64]int
}

func (r *excelBPSAccountStateRepo) SetError(_ context.Context, id int64, message string) error {
	if r.errors == nil {
		r.errors = map[int64]string{}
	}
	r.errors[id] = message
	return nil
}

func (r *excelBPSAccountStateRepo) SetModelRateLimit(_ context.Context, id int64, scope string, _ time.Time, _ ...string) error {
	if r.modelLimits == nil {
		r.modelLimits = map[int64][]string{}
	}
	r.modelLimits[id] = append(r.modelLimits[id], scope)
	return nil
}

func (r *excelBPSAccountStateRepo) SetTempUnschedulable(_ context.Context, id int64, _ time.Time, _ string) error {
	if r.tempUnschedulable == nil {
		r.tempUnschedulable = map[int64]int{}
	}
	r.tempUnschedulable[id]++
	return nil
}

func (r *excelBPSAccountStateRepo) SetOverloaded(_ context.Context, id int64, until time.Time) error {
	if r.overloaded == nil {
		r.overloaded = map[int64]time.Time{}
	}
	r.overloaded[id] = until
	return nil
}

func forwardExcelBPSWithAccountPolicy(t *testing.T, status int, body string, configure ...func(*Account)) (*httptest.ResponseRecorder, *excelBPSAccountStateRepo, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}}
	svc := openAIClientToolsTestService(upstream)
	repo := &excelBPSAccountStateRepo{}
	svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
	request, err := buildExcelBPSAccountTestBody("gpt-5.6-sol", "hi")
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request))
	account := excelAccount()
	for _, apply := range configure {
		apply(account)
	}
	_, err = svc.Forward(context.Background(), c, account, request)
	require.Len(t, upstream.requests, 1)
	return rec, repo, err
}

// A BPS 402 is an account-level failure like on the native path: a deactivated
// workspace marks the account as an error and the request moves on.
func TestExcelBPS402UsesAccountPolicyAndFailsOver(t *testing.T) {
	for name, body := range map[string]string{
		"deactivated workspace": `{"detail":{"code":"deactivated_workspace","message":"diagnostic Bearer test-token"}}`,
		"payment required":      `{"error":{"code":"payment_required","message":"diagnostic Bearer test-token"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec, repo, err := forwardExcelBPSWithAccountPolicy(t, http.StatusPaymentRequired, body)

			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, ExcelBPSAccountRejectedReason, failover.Reason)
			require.Equal(t, http.StatusPaymentRequired, failover.StatusCode)
			require.True(t, failover.ShouldRetryNextAccount())
			require.Empty(t, failover.ResponseBody)
			require.Zero(t, rec.Body.Len(), "leave the response to the handler")
			// The admin-facing account error keeps a redacted diagnostic only.
			require.Contains(t, repo.errors, int64(300))
			require.NotContains(t, repo.errors[300], "test-token")
			require.NotContains(t, failover.ClientMessage, "diagnostic")
		})
	}
}

// A BPS 5xx/529 answered before any output moves on to another account; 529
// also marks the account overloaded, as on the native path.
func TestExcelBPS5xxFailsOver(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, 529} {
		rec, repo, err := forwardExcelBPSWithAccountPolicy(t, status, `{"error":{"message":"private upstream diagnostic"}}`)

		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover, "status %d", status)
		require.Equal(t, ExcelBPSUpstreamErrorReason, failover.Reason)
		require.Equal(t, status, failover.StatusCode)
		require.True(t, failover.ShouldRetryNextAccount())
		require.False(t, failover.IsCredentialFailure())
		require.Empty(t, failover.ResponseBody)
		require.NotContains(t, failover.ClientMessage, "private")
		require.Zero(t, rec.Body.Len(), "leave the response to the handler")
		require.Empty(t, repo.errors)
		if status == 529 {
			require.Equal(t, http.StatusServiceUnavailable, failover.ClientStatusCode)
			require.Contains(t, repo.overloaded, int64(300))
		} else {
			require.Equal(t, http.StatusBadGateway, failover.ClientStatusCode)
			require.Empty(t, repo.overloaded)
		}
	}
}

// Other 4xx describe this request: they still answer directly.
func TestExcelBPSRequestErrorsDoNotFailOver(t *testing.T) {
	rec, repo, err := forwardExcelBPSWithAccountPolicy(t, http.StatusNotFound, `{"error":{"message":"private upstream diagnostic"}}`)

	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NotContains(t, rec.Body.String(), "private")
	require.Empty(t, repo.errors)
}

// BPS-only accounts expose their ChatGPT workspace for account bookkeeping
// (team fan-out, BPS headers), but never receive native Codex headers.
func TestBPSOAuthChatGPTAccountIdentity(t *testing.T) {
	account := excelAccount()
	account.Credentials["chatgpt_account_is_fedramp"] = true
	require.Equal(t, "test-account", account.GetChatGPTAccountID())
	require.True(t, account.IsChatGPTAccountFedRAMP())
	require.Equal(t, "test-account", excelBPSAccountID(account, ""))

	headers := http.Header{}
	setOpenAIChatGPTAccountHeaders(headers, account)
	require.Empty(t, headers.Get("chatgpt-account-id"))
	require.Empty(t, headers.Get("x-openai-fedramp"))

	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"chatgpt_account_id": "x"}}
	require.Empty(t, apiKey.GetChatGPTAccountID())
}

// Like the native path, the BPS account policy knows the upstream model, so a
// default-scope temporary-unschedulable rule cools only (account, model).
func TestExcelBPS5xxTempUnschedulableRuleIsModelScoped(t *testing.T) {
	rec, repo, err := forwardExcelBPSWithAccountPolicy(t, http.StatusInternalServerError, `{"error":{"message":"server overloaded"}}`, func(account *Account) {
		account.Credentials["temp_unschedulable_enabled"] = true
		account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{"error_code": 500, "keywords": []any{"overloaded"}, "duration_minutes": 5}}
	})

	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, ExcelBPSUpstreamErrorReason, failover.Reason)
	require.Zero(t, rec.Body.Len())
	require.Equal(t, []string{"gpt-5.6-sol"}, repo.modelLimits[300])
	require.Zero(t, repo.tempUnschedulable[300], "the account's other models stay schedulable")
}
