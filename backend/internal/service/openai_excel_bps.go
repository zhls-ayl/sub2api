package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var excelBPSReplay basispoints.ReplayCache
var excelBPSCatalog basispoints.CatalogCache

// handleExcelBPSUnauthorized hands a BPS 401 to the account-level policy. The
// Excel token is the account's only credential, so the shared OpenAI OAuth 401
// rules apply: invalidate the cached token and pause for a refresh window, or
// mark revoked tokens as errors.
func (s *OpenAIGatewayService) handleExcelBPSUnauthorized(ctx context.Context, account *Account, status int, headers http.Header, raw []byte) {
	if status != http.StatusUnauthorized {
		return
	}
	if s.rateLimitService == nil {
		return
	}
	fields := map[string]string{"message": "Excel BPS authentication failed"}
	code := extractUpstreamErrorCode(raw)
	if code == "token_invalidated" || code == "token_revoked" {
		fields["code"] = code
	}
	authError := map[string]any{"error": fields}
	if gjson.GetBytes(raw, "detail").String() == "Unauthorized" {
		authError["detail"] = "Unauthorized"
	}
	body, _ := json.Marshal(authError)
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	s.rateLimitService.HandleUpstreamError(stateCtx, account, status, headers, body)
}

// handleExcelBPSForbidden hands a generic BPS 403 (not a model-permission
// rejection) to the account-level policy. A BPS-only account has no other
// channel, so the shared OpenAI 403 rules apply to the whole account: HTML or
// Cloudflare block pages carry no penalty, other rejections cool the account
// down and escalate to an error after repeated hits. BPS bodies can echo
// request data, so only the code and a redacted message are passed on.
func (s *OpenAIGatewayService) handleExcelBPSForbidden(ctx context.Context, account *Account, headers http.Header, raw []byte, token string) {
	if s.rateLimitService == nil {
		return
	}
	body := raw
	if !isHTMLResponse(raw) && !isCloudflareBotBlockResponse(raw) {
		fields := map[string]string{"message": "Excel BPS rejected the account"}
		if code := extractUpstreamErrorCode(raw); bpsTestDiagnosticIdentifier.MatchString(code) {
			fields["code"] = code
		}
		message := strings.TrimSpace(extractUpstreamErrorMessage([]byte(excelBPSSanitizeErrorBody(string(raw), token, account))))
		if message != "" && (token == "" || !strings.Contains(message, token)) {
			fields["message"] = "Excel BPS rejected the account: " + truncateString(message, 256)
		}
		body, _ = json.Marshal(map[string]any{"error": fields})
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	s.rateLimitService.HandleUpstreamError(stateCtx, account, http.StatusForbidden, headers, body)
}

// getExcelBPSAccessToken resolves the account's own OAuth token: without the
// dual-grant machinery the Excel session lives in accounts.credentials and the
// generic token provider refreshes it via credentials.client_id.
func (s *OpenAIGatewayService) getExcelBPSAccessToken(ctx context.Context, account *Account) (string, error) {
	token, _, err := s.GetAccessToken(ctx, account)
	return token, err
}

func excelBPSAccountID(account *Account, accessToken string) string {
	if accountID := strings.TrimSpace(account.GetChatGPTAccountID()); accountID != "" {
		return accountID
	}
	claims, err := openai.DecodeIDToken(accessToken)
	if err != nil || claims.OpenAIAuth == nil {
		return ""
	}
	return strings.TrimSpace(claims.OpenAIAuth.ChatGPTAccountID)
}

func newExcelBPSRequest(ctx context.Context, body []byte, token, accountID string) (*http.Request, error) {
	return newExcelBPSRequestTo(ctx, basispoints.ResponsesURL, "text/event-stream", body, token, accountID)
}

func newExcelBPSRequestTo(ctx context.Context, targetURL, accept string, body []byte, token, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"Authorization": {"Bearer " + token}, "Chatgpt-Account-Id": {accountID}, "X-Openai-Account-Id": {accountID},
		"X-Basispoints-Auth-Mode": {"chatgpt"}, "Content-Type": {"application/json"}, "Accept": {accept},
		"Origin": {"https://bps.openai.com"}, "User-Agent": {"Mozilla/5.0"},
		"X-Openai-Internal-Basispoints-Client-Product":       {"basispoints-excel-plugin"},
		"X-Openai-Internal-Basispoints-Client-Agent-Profile": {"excel"},
	}
	return req, nil
}

// BPS deliberately bypasses Codex ticket/cookie injection and OAuth plugins:
// only the selected account's bearer and ChatGPT account ID belong on this host.
func (s *OpenAIGatewayService) forwardExcelBPS(ctx context.Context, c *gin.Context, account *Account, body []byte, start time.Time) (forwardResult *OpenAIForwardResult, forwardErr error) {
	fail := func(status int, code, message string, param ...string) (*OpenAIForwardResult, error) {
		// A compact keepalive may already have committed SSE headers. Otherwise
		// finish a single JSON response so the handler cannot append another error.
		committed := StopOpenAICompactSSEKeepaliveCommitted(c)
		MarkResponseCommitted(c)
		if committed {
			failureParam := ""
			if len(param) > 0 {
				failureParam = param[0]
			}
			writeOpenAICompactSSEFailureMessageParam(c, status, code, message, failureParam)
		} else {
			errorType := "invalid_request_error"
			if status >= 500 {
				errorType = "server_error"
			}
			errorBody := gin.H{"type": errorType, "code": code, "message": message}
			if len(param) > 0 && param[0] != "" {
				errorBody["param"] = param[0]
			}
			c.JSON(status, gin.H{"error": errorBody})
		}
		return nil, &excelBPSForwardError{code: code}
	}
	originalModel := gjson.GetBytes(body, "model").String()
	model := account.GetMappedModel(originalModel)
	stream := gjson.GetBytes(body, "stream").Bool()
	clientCanceled := func() (*OpenAIForwardResult, error) {
		StopOpenAICompactSSEKeepaliveCommitted(c)
		MarkResponseCommitted(c)
		MarkOpsClientCancellation(c, stream)
		// No response or metered usage exists before headers; do not create a usage row.
		return nil, context.Canceled
	}
	// No output exists yet, so the handler may replay the request on another
	// account within its switch budget unless the client is already gone.
	failoverRateLimited := func(retryAfter string) (*OpenAIForwardResult, error) {
		s.coolDownExcelBPS(ctx, account, retryAfter)
		if isExcelBPSClientCancellation(c, ctx.Err()) {
			return clientCanceled()
		}
		return nil, newExcelBPSRateLimitedFailoverError(retryAfter)
	}
	// A generic BPS 401/403 disqualifies this BPS-only account as a whole;
	// nothing was written yet, so the handler may retry another account.
	failoverAccountRejected := func(status int) (*OpenAIForwardResult, error) {
		if isExcelBPSClientCancellation(c, ctx.Err()) {
			return clientCanceled()
		}
		return nil, newExcelBPSAccountRejectedFailoverError(status)
	}
	// A model-permission 403 only cools this (account, model); nothing was
	// written yet, so another account may still serve the model.
	failoverModelNotAllowed := func() (*OpenAIForwardResult, error) {
		if isExcelBPSClientCancellation(c, ctx.Err()) {
			return clientCanceled()
		}
		return nil, newExcelBPSModelNotAllowedFailoverError()
	}
	// Images BPS cannot carry (size, count, format or reference rules) are a
	// property of the request: no BPS account would take them, but a native
	// account may. Nothing was sent yet.
	failoverImageUnsupported := func(err error) (*OpenAIForwardResult, error) {
		if isExcelBPSClientCancellation(c, ctx.Err()) {
			return clientCanceled()
		}
		return nil, newExcelBPSImageUnsupportedFailoverError(err.Error())
	}
	// Ciphertext BPS already rejected in this conversation (a native account's
	// reasoning or compaction, say) is stripped up front, as on the native path,
	// so later turns are not rejected first. BPS keeps its own lineage namespace:
	// native accounts can still read native ciphertext.
	lineageEntryBody := body
	lineageGroupID := getOpenAIGroupIDFromContext(c)
	lineageSessionHash := ""
	excelBPSLineageSessionHash := func() string {
		if lineageSessionHash == "" {
			if hash := s.GenerateSessionHash(c, lineageEntryBody); hash != "" {
				lineageSessionHash = excelBPSLineageSessionPrefix + hash
			}
		}
		return lineageSessionHash
	}
	if store := s.getOpenAIWSStateStore(); store != nil && store.HasAnySessionInvalidEncryptedContent() {
		if invalid := s.sessionInvalidEncryptedContentDigests(lineageGroupID, excelBPSLineageSessionHash()); len(invalid) > 0 {
			body, _ = s.stripSessionInvalidEncryptedContentLogged(body, invalid, "excel_bps_invalid_encrypted_lineage_strip", account.ID, 0)
		}
	}
	body, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return fail(400, "basispoints_request_invalid", "Invalid model request")
	}
	identity, transient := resolveExcelBPSIdentity(c, body, getAPIKeyIDFromContext(c), false)
	if identity != "" {
		body, err = sjson.SetBytes(body, "prompt_cache_key", identity)
		if err != nil {
			return nil, err
		}
	}
	if isOpenAIResponsesCompactPath(c) {
		var request map[string]any
		if err = json.Unmarshal(body, &request); err != nil {
			return fail(400, "basispoints_request_invalid", "Invalid compact request")
		}
		var input []any
		switch v := request["input"].(type) {
		case []any:
			input = v
		case string:
			input = []any{map[string]any{"role": "user", "content": v}}
		default:
			return fail(400, "basispoints_request_invalid", "Compact requires input")
		}
		request["input"] = append(input, map[string]any{"type": "compaction_trigger"})
		request["tool_choice"] = "none"
		body, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
	}
	scope := fmt.Sprintf("account:%d/key:%d/thread:%s", account.ID, getAPIKeyIDFromContext(c), identity)
	if transient {
		scope = "transient:" + scope
	}
	if err = basispoints.ValidateNewAgentMessage(body); err != nil {
		return fail(400, "basispoints_request_invalid", err.Error())
	}
	if account.IsExcelBPSIgnoreEncryptedContentEnabled() {
		body, err = basispoints.StripEncryptedContent(body)
		if err != nil {
			return fail(400, "basispoints_request_invalid", err.Error())
		}
	}
	// Inline images are checked as a batch before any upload; the plan carries
	// placeholder file IDs until every local protocol check has passed.
	images, err := basispoints.PrepareNativeImages(body)
	if err != nil {
		return failoverImageUnsupported(err)
	}
	var replay *basispoints.ReplayCache
	var catalog *basispoints.CatalogCache
	if identity != "" {
		replay, catalog = &excelBPSReplay, &excelBPSCatalog
		if s.cfg != nil && s.cfg.Gateway.ExcelBPSStateRedisEnabled && !transient {
			store, ok := s.cache.(basispoints.StateStore)
			if !ok {
				return fail(503, "basispoints_state_unavailable", basispoints.ErrStateUnavailable.Error())
			}
			replay, catalog = basispoints.NewPersistentCaches(ctx, store)
		}
	}
	upstreamBody, bridge, err := images.PrepareWithCatalog(scope, replay, catalog)
	if err != nil {
		if errors.Is(err, basispoints.ErrStateUnavailable) {
			return fail(503, "basispoints_state_unavailable", basispoints.ErrStateUnavailable.Error())
		}
		var contentErr *basispoints.ContentValidationError
		if errors.As(err, &contentErr) {
			if contentErr.ContentType == "input_image" {
				return failoverImageUnsupported(err)
			}
			return fail(400, "basispoints_request_invalid", err.Error(), contentErr.Path)
		}
		return fail(400, "basispoints_request_invalid", err.Error())
	}
	token, err := s.getExcelBPSAccessToken(ctx, account)
	if err != nil {
		if isExcelBPSClientCancellation(c, err) {
			return clientCanceled()
		}
		return fail(502, "basispoints_auth_unavailable", "Excel OAuth credential is unavailable")
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return fail(400, "basispoints_account_id_missing", "Excel BPS requires chatgpt_account_id")
	}
	if images.HasImages() {
		// Attachment IDs are scoped to the conversation, account and credential;
		// requests without a conversation identity never reuse an upload.
		attachmentScope := ""
		if identity != "" {
			attachmentScope = scope + "\x00" + accountID + "\x00" + token
		}
		attachmentProxy := ""
		if account.Proxy != nil {
			attachmentProxy = account.Proxy.URL()
		}
		body, err = images.Upload(ctx, &s.excelBPSAttachments, attachmentScope, func(uploadCtx context.Context, img basispoints.InlineAttachment) (string, error) {
			SetActualOpenAIUpstreamEndpoint(c, "/basispoints/api/attachments")
			return s.uploadExcelBPSAttachment(uploadCtx, account, token, accountID, attachmentProxy, model, img)
		})
		if err != nil {
			if isExcelBPSClientCancellation(c, err) {
				return clientCanceled()
			}
			status := http.StatusBadGateway
			var uploadError *excelBPSAttachmentError
			if errors.As(err, &uploadError) {
				status = uploadError.status
			}
			if errors.Is(err, basispoints.ErrAttachmentBusy) {
				status = http.StatusServiceUnavailable
			}
			// Nothing was generated yet, so the Responses rules before output
			// apply: 429 cools only the BPS route, 401/402 disqualify the account
			// and 5xx/529 follow the account policy (all recorded by the upload),
			// and each moves the request to another account. A transport failure
			// moves on too: resending an upload only leaves an unused attachment.
			// A 403 and anything else answer directly (see uploadExcelBPSAttachment).
			if uploadError != nil {
				switch {
				case status == http.StatusTooManyRequests:
					recordExcelBPSAttachmentFailure(ctx, c, account, err, true)
					return failoverRateLimited(uploadError.retryAfter)
				case status == http.StatusUnauthorized || status == http.StatusPaymentRequired:
					recordExcelBPSAttachmentFailure(ctx, c, account, err, true)
					return failoverAccountRejected(status)
				case uploadError.stage == "attachment_http" && uploadError.upstreamStatus >= 500,
					uploadError.stage == "attachment_transport":
					recordExcelBPSAttachmentFailure(ctx, c, account, err, true)
					if isExcelBPSClientCancellation(c, ctx.Err()) {
						return clientCanceled()
					}
					return nil, newExcelBPSUpstreamFailoverError(status)
				}
			}
			recordExcelBPSAttachmentFailure(ctx, c, account, err, false)
			return fail(status, "basispoints_attachment_error", "Excel BPS attachment upload failed; request was not replayed and account scheduling was not changed")
		}
		upstreamBody, bridge, err = bridge.Reprepare(body)
		if err != nil {
			return fail(400, "basispoints_request_invalid", err.Error())
		}
	}
	requestCtx := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileExcelBPS))

	SetActualOpenAIUpstreamEndpoint(c, "/basispoints/api/responses")
	SetOpsUpstreamModel(c, model)
	sent := time.Now()
	resp, lease, proxyURL, err := s.doExcelBPSRequest(requestCtx, c, account, scope, upstreamBody, token, accountID, nil)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(sent).Milliseconds())
	if err != nil {
		if isExcelBPSClientCancellation(c, err) {
			return clientCanceled()
		}
		if errors.Is(err, errExcelBPSProxyUnavailable) {
			return fail(503, "basispoints_proxy_unavailable", "No healthy BPS session proxy is available; retry later")
		}
		return fail(502, "basispoints_transport_error", "Excel BPS connection failed; request was not replayed after sending")
	}
	if lease != nil {
		defer lease.Release()
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		observeBPSTestResponse(ctx, resp, model, token, "")
	}
	if resp.StatusCode == http.StatusBadRequest {
		// Read and close before retrying: the HTTP body owns the account's
		// concurrency slot. Keep the proxy lease for the exact same exit.
		const maxRejectionBytes = 512 << 10
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRejectionBytes+1))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if retryBody, retry := prepareExcelBPSInvalidEncryptedRetry(upstreamBody, raw); retry && readErr == nil && len(raw) <= maxRejectionBytes && ctx.Err() == nil {
			retryReq, retryErr := newExcelBPSRequest(requestCtx, retryBody, token, accountID)
			if retryErr != nil {
				return fail(502, "basispoints_transport_error", "Excel BPS recovery request could not be prepared")
			}
			// Remember the rejected ciphertext for this conversation (see the
			// lineage strip above), as the native path does.
			if digests := collectOpenAIEncryptedContentDigestsRaw(lineageEntryBody); len(digests) > 0 {
				s.markOpenAIWSInvalidEncryptedContentLineage(lineageGroupID, excelBPSLineageSessionHash(), digests)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
				ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
				UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
				UpstreamURL: basispoints.ResponsesURL, Kind: "invalid_encrypted_content_retry",
				Message: "Excel BPS rejected encrypted reasoning; retrying once without opaque reasoning on the same route",
			})
			logger.LegacyPrintf("service.openai_excel_bps", "retrying invalid encrypted reasoning once: account_id=%d", account.ID)
			c.Set("excel_bps_upstream_attempt", c.GetInt("excel_bps_upstream_attempt")+1)
			// Do not re-enter proxy acquisition or transport retries after sending.
			resp, err = s.httpUpstream.Do(retryReq, proxyURL, account.ID, account.Concurrency)
			if err == nil {
				s.guardExcelBPSProgress(requestCtx, resp)
			}
			SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(sent).Milliseconds())
			if err != nil {
				if isExcelBPSClientCancellation(c, err) {
					return clientCanceled()
				}
				if lease != nil && ctx.Err() == nil {
					lease.ReportFailure()
				}
				return fail(502, "basispoints_transport_error", "Excel BPS recovery connection failed; request was not replayed again")
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				observeBPSTestResponse(ctx, resp, model, token, "")
			}
			upstreamBody = retryBody
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if lease != nil && resp.StatusCode >= 500 && ctx.Err() == nil {
			lease.ReportUpstreamFailure()
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		observeBPSTestFailure(ctx, resp, model, token, raw, account)
		// BPS throttles its own endpoint. A BPS 429 must not write Codex
		// quota/cooldown state; it cools only the BPS route and fails over.
		// Preserve the original rejection for Ops without exposing it to clients.
		// BPS errors can echo request fields, so redact before storing diagnostics.
		upstreamMessage := fmt.Sprintf("Excel BPS returned HTTP %d", resp.StatusCode)
		upstreamDetail := ""
		if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
			safeBody := excelBPSSanitizeErrorBody(string(raw), token, account)
			maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
			if maxBytes <= 0 {
				maxBytes = 2048
			}
			upstreamDetail, _ = sanitizeErrorBodyForStorage(safeBody, maxBytes)
			if message := strings.TrimSpace(extractUpstreamErrorMessage([]byte(safeBody))); message != "" {
				upstreamMessage = truncateString(message, 2048)
			}
		}
		// A failover attempt is only an event; the handler records the final state.
		kind := "failover"
		if resp.StatusCode != http.StatusTooManyRequests {
			kind = "http_error"
			setOpsUpstreamError(c, resp.StatusCode, upstreamMessage, upstreamDetail)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
			UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
			UpstreamURL: basispoints.ResponsesURL, Kind: kind,
			Message: upstreamMessage, Detail: upstreamDetail, UpstreamResponseBody: upstreamDetail,
		})
		if resp.StatusCode == http.StatusTooManyRequests {
			return failoverRateLimited(resp.Header.Get("Retry-After"))
		}
		if resp.StatusCode == http.StatusUnauthorized {
			s.handleExcelBPSUnauthorized(ctx, account, resp.StatusCode, resp.Header, raw)
			return failoverAccountRejected(resp.StatusCode)
		}
		code := gjson.GetBytes(raw, "error.code").String()
		if isExcelBPSModelPermissionError(resp.StatusCode, code) {
			s.coolDownExcelBPSModel(account, model)
			return failoverModelNotAllowed()
		}
		if resp.StatusCode == http.StatusForbidden {
			s.handleExcelBPSForbidden(ctx, account, resp.Header, raw, token)
			return failoverAccountRejected(resp.StatusCode)
		}
		// 402 (e.g. a deactivated workspace) and 5xx/529 follow the native
		// account policy; nothing was generated, so another account may serve.
		if resp.StatusCode == http.StatusPaymentRequired {
			s.handleExcelBPSAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, raw, token, model)
			return failoverAccountRejected(resp.StatusCode)
		}
		if resp.StatusCode >= 500 {
			s.handleExcelBPSAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, raw, token, model)
			if isExcelBPSClientCancellation(c, ctx.Err()) {
				return clientCanceled()
			}
			return nil, newExcelBPSUpstreamFailoverError(resp.StatusCode)
		}
		message := "Excel BPS rejected this request; account scheduling was not changed"
		errorCode := "basispoints_upstream_error"
		if resp.StatusCode == http.StatusBadRequest && isExcelBPSInvalidEncryptedContent(raw) {
			errorCode = "invalid_encrypted_content"
			message = "Excel BPS could not verify encrypted conversation state; resend the original plaintext history or start a new conversation"
		}
		return fail(resp.StatusCode, errorCode, message)
	}
	// BPS and Codex share quota. Refresh at the HTTP boundary even if the client
	// disconnects or a later stream/protocol error prevents normal completion.
	s.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, resp.Header)
	converted := bridge.StreamWithRepairs(requestCtx, resp.Body, func(repairCtx context.Context, failed map[string]any, validation error) (map[string]any, error) {
		correctedBody, err := basispoints.BuildToolRepairRequest(upstreamBody, failed, validation)
		if err != nil {
			return nil, err
		}
		repairReq, err := newExcelBPSRequest(repairCtx, correctedBody, token, accountID)
		if err != nil {
			return nil, err
		}
		repairResp, err := s.httpUpstream.Do(repairReq, proxyURL, account.ID, account.Concurrency)
		if err == nil {
			s.guardExcelBPSProgress(repairCtx, repairResp)
		}
		if err != nil {
			if repairCtx.Err() != nil {
				return nil, repairCtx.Err()
			}
			return nil, fmt.Errorf("excel BPS correction connection failed")
		}
		defer func() { _ = repairResp.Body.Close() }()
		stop := context.AfterFunc(repairCtx, func() { _ = repairResp.Body.Close() })
		defer stop()
		if repairResp.StatusCode >= 200 && repairResp.StatusCode < 300 {
			observeBPSTestResponse(repairCtx, repairResp, model, token, "")
		}
		if repairResp.StatusCode < 200 || repairResp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(repairResp.Body, 512<<10))
			observeBPSTestFailure(repairCtx, repairResp, model, token, raw, account)
			if repairResp.StatusCode == http.StatusTooManyRequests {
				// Output was already accepted: cool the route, never replay the request.
				s.coolDownExcelBPS(repairCtx, account, repairResp.Header.Get("Retry-After"))
			}
			// Output was already accepted, so the request cannot move to another
			// account; only the account-level state is updated.
			s.handleExcelBPSUnauthorized(repairCtx, account, repairResp.StatusCode, repairResp.Header, raw)
			s.handleExcelBPSAccountUpstreamError(repairCtx, account, repairResp.StatusCode, repairResp.Header, raw, token, model)
			if isExcelBPSModelPermissionError(repairResp.StatusCode, gjson.GetBytes(raw, "error.code").String()) {
				s.coolDownExcelBPSModel(account, model)
			} else if repairResp.StatusCode == http.StatusForbidden {
				s.handleExcelBPSForbidden(repairCtx, account, repairResp.Header, raw, token)
			}
			return nil, fmt.Errorf("excel BPS correction returned HTTP %d", repairResp.StatusCode)
		}
		s.UpdateCodexUsageSnapshotFromHeaders(repairCtx, account.ID, repairResp.Header)
		upstreamBody = correctedBody
		return basispoints.ReadToolRepairResponse(repairResp.Body)
	}, func(repairCtx context.Context) (io.ReadCloser, error) {
		repairBody, err := basispoints.RepairRequest(upstreamBody)
		if err != nil {
			return nil, err
		}
		retry, err := newExcelBPSRequest(repairCtx, repairBody, token, accountID)
		if err != nil {
			return nil, err
		}
		repaired, err := s.httpUpstream.Do(retry, proxyURL, account.ID, account.Concurrency)
		if err == nil {
			s.guardExcelBPSProgress(repairCtx, repaired)
		}
		if err != nil {
			return nil, fmt.Errorf("excel BPS tool correction transport failed")
		}
		if repaired.StatusCode >= 200 && repaired.StatusCode < 300 {
			observeBPSTestResponse(repairCtx, repaired, model, token, "")
		}
		if repaired.StatusCode < 200 || repaired.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(repaired.Body, 512<<10))
			observeBPSTestFailure(repairCtx, repaired, model, token, raw, account)
			_ = repaired.Body.Close()
			if repaired.StatusCode == http.StatusTooManyRequests {
				s.coolDownExcelBPS(repairCtx, account, repaired.Header.Get("Retry-After"))
			}
			// Output was already accepted, so the request cannot move to another
			// account; only the account-level state is updated.
			s.handleExcelBPSUnauthorized(repairCtx, account, repaired.StatusCode, repaired.Header, raw)
			s.handleExcelBPSAccountUpstreamError(repairCtx, account, repaired.StatusCode, repaired.Header, raw, token, model)
			if isExcelBPSModelPermissionError(repaired.StatusCode, gjson.GetBytes(raw, "error.code").String()) {
				s.coolDownExcelBPSModel(account, model)
			} else if repaired.StatusCode == http.StatusForbidden {
				s.handleExcelBPSForbidden(repairCtx, account, repaired.Header, raw, token)
			}
			return nil, fmt.Errorf("excel BPS tool correction returned HTTP %d", repaired.StatusCode)
		}
		s.UpdateCodexUsageSnapshotFromHeaders(repairCtx, account.ID, repaired.Header)
		return repaired.Body, nil
	})
	defer func() { _ = converted.Close() }()
	// The bridge sees the body after group policy mapping. Keep the original
	// client effort for usage display, and the BPS-normalized effort for billing.
	requestedEffort := coalesceRequestedReasoningEffort(RequestedReasoningEffortFromContext(ctx), &bridge.RequestedEffort)
	result := &OpenAIForwardResult{Model: originalModel, UpstreamModel: model, UpstreamEndpoint: "/basispoints/api/responses", Stream: stream, ReasoningEffort: &bridge.Effort, RequestedReasoningEffort: requestedEffort, RequestID: resp.Header.Get("x-request-id")}
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	scanner := newOpenAISSEReadPump(converted, 16<<20)
	defer scanner.Close()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	keepalive := func() {
		if stream && ctx.Err() == nil {
			_, _ = c.Writer.WriteString(": keepalive\n\n")
			c.Writer.Flush()
		}
	}
	var completed []byte
	var upstreamFailure *basispoints.UpstreamFailure
	terminal := ""
	terminalSuccessful := false
	cacheCreationAsInput := account.IsExcelBPSCacheCreationAsInputEnabled()
	for scanner.Next(ctx, 0, heartbeat.C, keepalive) {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			payload := []byte(strings.TrimPrefix(line, "data: "))
			kind := gjson.GetBytes(payload, "type").String()
			s.parseSSEUsageBytes(payload, &result.Usage)
			if cacheCreationAsInput {
				payload, err = excelBPSDownstreamUsage(payload)
				if err != nil {
					return fail(http.StatusBadGateway, "basispoints_usage_invalid", "Excel BPS usage could not be normalized")
				}
				line = "data: " + string(payload)
			}
			if result.FirstTokenMs == nil && (kind == "response.output_text.delta" || kind == "response.output_item.added") {
				ms := int(time.Since(start).Milliseconds())
				result.FirstTokenMs = &ms
			}
			switch kind {
			case "response.completed", "response.failed", "response.cancelled", "response.incomplete", "error":
				if kind == "response.completed" && lease != nil {
					lease.ReportSuccess()
				}
				terminal = kind
				terminalSuccessful = IsSuccessfulStreamTerminal(payload)
				completed = []byte(gjson.GetBytes(payload, "response").Raw)
				result.ResponseID = gjson.GetBytes(payload, "response.id").String()
				result.UpstreamResponseModel = gjson.GetBytes(payload, "response.model").String()
				upstreamFailure = basispoints.ParseUpstreamFailure(payload)
				if upstreamFailure != nil {
					// Preserve the actual HTTP status separately from the event's
					// semantic status. An accepted generation is never replayed.
					setOpsUpstreamError(c, resp.StatusCode, upstreamFailure.Message, "")
					MarkOpsStreamErrorValue(c, OpsStreamError{
						ErrType: upstreamFailure.Type, Code: upstreamFailure.Code, Message: upstreamFailure.Message,
						IntendedStatus: upstreamFailure.Status, CountTowardsSLA: true, NonStream: !stream,
					})
					if upstreamFailure.Status == http.StatusTooManyRequests {
						s.coolDownExcelBPS(ctx, account, resp.Header.Get("Retry-After"))
					}
				}
			}
		}
		if stream {
			if _, err = c.Writer.WriteString(line + "\n"); err != nil {
				result.ClientDisconnect = true
				if isExcelBPSClientCancellation(c, c.Request.Context().Err()) {
					MarkOpsClientCancellation(c, stream)
				}
				result.Duration = time.Since(start)
				return result, err
			}
			if line == "" {
				c.Writer.Flush()
			}
		}
	}
	result.Duration = time.Since(start)
	result.UpstreamTerminalEvent = terminal
	if err = scanner.Err(); err != nil || terminal == "" {
		if ctx.Err() != nil {
			if isExcelBPSClientCancellation(c, ctx.Err()) {
				MarkOpsClientCancellation(c, stream)
			}
			result.ClientDisconnect = true
			return result, ctx.Err()
		}
		// Do not replay an incomplete response. Mark the exit for the next
		// request only; a client cancellation never penalizes the node.
		if lease != nil {
			lease.ReportStreamFailure()
		}
		recordExcelBPSTransportFailure(ctx, c, account, scope, proxyURL, err, "stream", c.GetInt("excel_bps_upstream_attempt"), false)
		MarkOpsStreamError(c, "basispoints_stream_incomplete", "Excel BPS stream ended before completion", http.StatusBadGateway)
		MarkResponseCommitted(c)
		if stream {
			_, _ = c.Writer.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"code\":\"basispoints_stream_incomplete\",\"message\":\"Upstream stream ended before completion\"}}}\n\n")
			c.Writer.Flush()
		} else {
			c.JSON(502, gin.H{"error": gin.H{"type": "server_error", "code": "basispoints_stream_incomplete", "message": "Excel BPS stream ended before completion"}})
		}
		return result, fmt.Errorf("excel BPS stream incomplete")
	}
	if terminal != "response.completed" {
		MarkResponseCommitted(c)
	}
	if !stream {
		if upstreamFailure != nil {
			if StopOpenAICompactSSEKeepaliveCommitted(c) {
				writeOpenAICompactSSEFailureMessage(c, upstreamFailure.Status, upstreamFailure.Code, upstreamFailure.Message)
			} else {
				c.JSON(upstreamFailure.Status, gin.H{"error": upstreamFailure.Details()})
			}
		} else if terminal != "response.completed" {
			c.JSON(502, gin.H{"error": gin.H{"code": "basispoints_protocol_error", "message": "Excel BPS did not complete the response"}})
		} else {
			c.Data(200, "application/json", completed)
		}
	}
	if terminal != "response.completed" {
		return result, fmt.Errorf("excel BPS terminal: %s", terminal)
	}
	s.bindHTTPResponseAccount(ctx, c, account, result.ResponseID)
	if stream && terminalSuccessful && !result.ClientDisconnect {
		MarkOpsStreamCompleted(c, account.ID)
	}
	return result, nil
}

var excelBPSBearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[^\s"',;<>]+`)
var excelBPSURLCredentialsPattern = regexp.MustCompile(`(https?://)[^/\s@]+@`)

// excelBPSAttachmentIDPattern hides uploaded attachment IDs that BPS errors may
// echo back.
var excelBPSAttachmentIDPattern = regexp.MustCompile(`\bfile-[A-Za-z0-9_-]+`)

func excelBPSSanitizeErrorBody(raw, token string, account *Account) string {
	if !json.Valid([]byte(raw)) {
		return ""
	}
	secrets := append([]string{token}, excelBPSAccountSecrets(account)...)
	fields := make(map[string]string)
	for _, key := range []string{"message", "code", "type", "param"} {
		value := gjson.Get(raw, "error."+key)
		if value.Type != gjson.String {
			continue
		}
		clean := value.String()
		for _, secret := range secrets {
			if secret != "" {
				clean = strings.ReplaceAll(clean, secret, "[redacted]")
			}
		}
		clean = excelBPSBearerPattern.ReplaceAllString(clean, "Bearer [redacted]")
		clean = excelBPSURLCredentialsPattern.ReplaceAllString(clean, "${1}[redacted]@")
		clean = excelBPSAttachmentIDPattern.ReplaceAllString(clean, "file-[redacted]")
		clean = sanitizeUpstreamErrorMessage(clean)
		fields[key] = truncateString(logredact.RedactText(clean, "authorization", "api_key", "apikey", "token", "secret", "key", "cookie", "ticket", "recovery_ticket"), 2048)
	}
	encoded, _ := json.Marshal(map[string]any{"error": fields})
	return string(encoded)
}

func excelBPSAccountSecrets(account *Account) []string {
	var secrets []string
	for _, key := range []string{"access_token", "refresh_token", "id_token", "api_key", "session_key", "cookie"} {
		if value := account.GetCredential(key); value != "" {
			secrets = append(secrets, value)
		}
	}
	if account.Proxy != nil && account.Proxy.Password != "" {
		secrets = append(secrets, account.Proxy.Password)
	}
	return secrets
}

// Explicit conversation identity remains sticky. Without session pools there
// is no transient per-request identity: unnamed requests share the default
// scope instead of pinning an exit.
func resolveExcelBPSIdentity(c *gin.Context, body []byte, apiKeyID int64, managed bool) (string, bool) {
	if identity, _ := resolveOpenAIWSExecutionScope(c, body, apiKeyID); identity != "" {
		return identity, false
	}
	return "", false
}
