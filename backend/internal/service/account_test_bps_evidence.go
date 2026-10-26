package service

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type bpsTestResponseObserverKey struct{}
type bpsTestResponseObserver func(TestEvent)

var bpsTestDiagnosticIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,160}$`)

// This observer exists only on an administrator's explicit test request.
func (s *AccountTestService) withBPSTestEvidence(ctx context.Context, c *gin.Context) context.Context {
	return context.WithValue(ctx, bpsTestResponseObserverKey{}, bpsTestResponseObserver(func(event TestEvent) { s.sendEvent(c, event) }))
}

func observeBPSTestResponse(ctx context.Context, resp *http.Response, model, token, code string) {
	observeBPSTestEvent(ctx, resp, model, token, code, "")
}

// bpsTestErrorMessageMaxLen bounds the BPS rejection message shown in a test.
const bpsTestErrorMessageMaxLen = 512

// observeBPSTestFailure reports a non-2xx BPS response together with BPS's own
// error message, so a failing admin test shows why BPS rejected the request
// without digging through ops logs. The message is redacted of the request
// token and account secrets; only explicit admin tests install the observer.
func observeBPSTestFailure(ctx context.Context, resp *http.Response, model, token string, raw []byte, account *Account) {
	if _, ok := ctx.Value(bpsTestResponseObserverKey{}).(bpsTestResponseObserver); !ok || resp == nil {
		return
	}
	message := strings.TrimSpace(extractUpstreamErrorMessage([]byte(excelBPSSanitizeErrorBody(string(raw), token, account))))
	if token != "" && strings.Contains(message, token) {
		message = ""
	}
	observeBPSTestEvent(ctx, resp, model, token, gjson.GetBytes(raw, "error.code").String(), truncateString(message, bpsTestErrorMessageMaxLen))
}

func observeBPSTestEvent(ctx context.Context, resp *http.Response, model, token, code, message string) {
	observer, ok := ctx.Value(bpsTestResponseObserverKey{}).(bpsTestResponseObserver)
	if !ok || resp == nil {
		return
	}
	clean := func(value string) string {
		value = strings.TrimSpace(value)
		if !bpsTestDiagnosticIdentifier.MatchString(value) || (token != "" && strings.Contains(value, token)) {
			return ""
		}
		return value
	}
	observer(TestEvent{Type: "upstream_response", UpstreamStatus: resp.StatusCode, UpstreamModel: clean(model), RequestID: clean(resp.Header.Get("x-request-id")), UpstreamErrorCode: clean(code), UpstreamErrorMessage: message})
}
