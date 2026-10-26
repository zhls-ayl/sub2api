package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ExcelBPSUpstreamErrorReason marks a BPS 5xx/529 answered before any output.
// Nothing was generated, so the handler may replay the request on another
// account, as the native path does for the same statuses.
const ExcelBPSUpstreamErrorReason = GatewayFailureReason("basispoints_upstream_error")

const excelBPSUpstreamErrorClientMessage = "Excel BPS upstream is temporarily unavailable, please retry later"

// newExcelBPSUpstreamFailoverError never carries the BPS body: it may echo
// request data.
func newExcelBPSUpstreamFailoverError(status int) *UpstreamFailoverError {
	clientStatus := http.StatusBadGateway
	if status == 529 {
		clientStatus = http.StatusServiceUnavailable
	}
	return &UpstreamFailoverError{
		StatusCode:        status,
		Stage:             GatewayFailureStageInference,
		Scope:             GatewayFailureScopeAccount,
		Reason:            ExcelBPSUpstreamErrorReason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  clientStatus,
		ClientMessage:     excelBPSUpstreamErrorClientMessage,
	}
}

// ExcelBPSImageUnsupportedReason marks a request whose images BPS cannot carry
// (size, count, format or reference rules of the protocol). The images, not
// the account, are the problem: the handler stops offering this request to
// BPS accounts and lets a native account try it. Exhausted, the client gets
// the protocol's own 400.
const ExcelBPSImageUnsupportedReason = GatewayFailureReason("basispoints_image_unsupported")

// newExcelBPSImageUnsupportedFailoverError carries only protocol-generated
// text (limits and an input path), never image data.
func newExcelBPSImageUnsupportedFailoverError(message string) *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:        http.StatusBadRequest,
		Stage:             GatewayFailureStageInference,
		Scope:             GatewayFailureScopeRequest,
		Reason:            ExcelBPSImageUnsupportedReason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusBadRequest,
		ClientMessage:     message,
	}
}

// handleExcelBPSAccountUpstreamError hands a BPS 402 or 5xx to the same
// account-level policy as the native path (handleOpenAIAccountUpstreamError):
// a deactivated workspace marks the account as an error, 529 marks it
// overloaded and other 5xx are only logged. model is the upstream (mapped)
// model, as on the native path, so model-scoped temporary-unschedulable rules
// cool only (account, model). BPS bodies can echo request data, so only
// identifier-like codes (error.code, detail.code) and a redacted message are
// passed on.
func (s *OpenAIGatewayService) handleExcelBPSAccountUpstreamError(ctx context.Context, account *Account, status int, headers http.Header, raw []byte, token, model string) {
	if s == nil || account == nil || (status != http.StatusPaymentRequired && status < 500) {
		return
	}
	fields := map[string]string{"message": fmt.Sprintf("Excel BPS returned HTTP %d", status)}
	if code := extractUpstreamErrorCode(raw); bpsTestDiagnosticIdentifier.MatchString(code) {
		fields["code"] = code
	}
	if message := strings.TrimSpace(extractUpstreamErrorMessage([]byte(excelBPSSanitizeErrorBody(string(raw), token, account)))); message != "" && (token == "" || !strings.Contains(message, token)) {
		fields["message"] = fmt.Sprintf("Excel BPS returned HTTP %d: %s", status, truncateString(message, 256))
	}
	safe := map[string]any{"error": fields}
	if code := gjson.GetBytes(raw, "detail.code").String(); bpsTestDiagnosticIdentifier.MatchString(code) {
		safe["detail"] = map[string]string{"code": code}
	}
	body, _ := json.Marshal(safe)
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	s.handleOpenAIAccountUpstreamError(stateCtx, account, status, headers, body, model)
}

// ExcelBPSFailoverClientError maps an exhausted BPS failover to its fixed
// client error. BPS rejections may echo request data, so neither the upstream
// body nor passthrough rules apply; it also keeps BPS account rejections out
// of the generic credential-failure message.
func ExcelBPSFailoverClientError(failoverErr *UpstreamFailoverError) (status int, errType, code, message string, ok bool) {
	if failoverErr == nil {
		return 0, "", "", "", false
	}
	status, message = failoverErr.ClientStatusCode, strings.TrimSpace(failoverErr.ClientMessage)
	code = string(failoverErr.Reason)
	switch failoverErr.Reason {
	case ExcelBPSRateLimitedReason:
		errType = "rate_limit_error"
	case ExcelBPSModelNotAllowedReason:
		errType = "invalid_request_error"
	case ExcelBPSImageUnsupportedReason:
		errType, code = "invalid_request_error", "basispoints_request_invalid"
	case ExcelBPSAccountRejectedReason, ExcelBPSUpstreamErrorReason, ExcelOAuthRouteUnavailableReason, BPSUnsupportedInboundReason:
		errType = "upstream_error"
	default:
		return 0, "", "", "", false
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	if message == "" {
		message = "Excel BPS request failed"
	}
	return status, errType, code, message, true
}
