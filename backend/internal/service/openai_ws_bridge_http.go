package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ws_bridge（WebSocket 桥接）：与 http_bridge 对称，上游统一走 WS。
// WS 入站与 ctx_pool 完全一致；本文件只处理 HTTP 入站——流式 /v1/responses
// 请求经上下文池的上游 WSv2 发出（Forward → forwardOpenAIWSV2），以及让该
// 路径在切号、保活、响应头与终态事件上对齐 HTTP 上游路径的行为。

// shouldUseOpenAIWSBridgeForHTTPIngress 判定 HTTP 入站请求是否按 ws_bridge 走上游 WS。
// 任一条件不满足时保持原行为（HTTP 入站一律走上游 HTTP）。
func (s *OpenAIGatewayService) shouldUseOpenAIWSBridgeForHTTPIngress(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	decision OpenAIWSProtocolDecision,
	body []byte,
	responsesLite bool,
) bool {
	reason := s.openAIWSBridgeHTTPIngressRejectReason(ctx, c, account, decision, body, responsesLite)
	if reason == "" {
		return true
	}
	// 只为配置成 ws_bridge 的账号记录拒绝原因，其他账号本就不走桥接。
	if s != nil && s.cfg != nil && account != nil &&
		account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) == OpenAIWSIngressModeWSBridge {
		if ctx == nil {
			ctx = context.Background()
		}
		logger.FromContext(ctx).Info("openai.ws_bridge_http_rejected",
			zap.Int64("account_id", account.ID),
			zap.String("reason", reason),
			zap.String("decision_transport", string(decision.Transport)),
			zap.String("decision_reason", decision.Reason),
		)
	}
	return false
}

// openAIWSBridgeHTTPIngressRejectReason 返回 HTTP 入站请求不走 ws_bridge 的原因；空串表示放行。
func (s *OpenAIGatewayService) openAIWSBridgeHTTPIngressRejectReason(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	decision OpenAIWSProtocolDecision,
	body []byte,
	responsesLite bool,
) string {
	if s == nil || s.cfg == nil || c == nil || account == nil {
		return "missing_context"
	}
	if GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP {
		return "client_transport_not_http"
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	if !wsCfg.ModeRouterV2Enabled {
		return "mode_router_v2_disabled"
	}
	if account.ResolveOpenAIResponsesWebSocketV2Mode(wsCfg.IngressModeDefault) != OpenAIWSIngressModeWSBridge {
		return "mode_not_ws_bridge"
	}
	// 协议决策器仍是总闸：WS 总开关、oauth/apikey 开关、force_http、并发校验照常生效。
	if decision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		return "decision_not_ws_v2"
	}
	// WS 只能向 /responses 发 response.create：compact、/{id}/cancel 等子路径与原生压缩 v2
	// 依赖 HTTP 端点语义。Lite 请求头由 forwardOpenAIWSV2 转成 client_metadata 标记。
	if openAIResponsesRequestPathSuffix(c) != "" {
		return "responses_subpath"
	}
	if IsOpenAINativeCompactionV2(c) {
		return "native_compaction_v2"
	}
	values := gjson.GetManyBytes(body, "stream", "background", "previous_response_id")
	// 仅流式：WS 分支对非流式客户端未实现 output 重建与 response.failed 切号。
	// background 会被 WS payload 丢弃，客户端会拿到同步响应而非排队响应。
	if !values[0].Bool() {
		return "not_stream"
	}
	if values[1].Bool() {
		return "background"
	}
	if isOpenAICompatMessagesBridgeContext(c) || isOpenAICompatMessagesBridgeBody(body) {
		return "compat_messages_bridge"
	}
	if account.IsOpenAIPassthroughEnabled() {
		return "passthrough_enabled"
	}
	// 插件接管的 OAuth 账号必须经插件出站（WS 入站对它们同样强制 http_bridge）。
	if s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(account) {
		return "plugin_route"
	}
	// 与 WS 入站一致：首包超过 HTTP 桥接阈值且无续接时仍走 HTTP。
	if s.shouldBridgeOpenAIWSHTTP(account, len(body), strings.TrimSpace(values[2].String())) {
		return "http_bridge_threshold"
	}
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return "raw_chat_completions"
	}
	if s.resolveUpstreamProtocolFor(ctx, account, APIProtocolResponses, upstreamRoutingModel(account, body, "")) != APIProtocolResponses {
		return "upstream_protocol_not_responses"
	}
	return ""
}

// startOpenAIWSBridgeKeepalive 在首个语义输出前按 gateway.stream_keepalive_interval
// 写 SSE 注释保活，避免长推理期间反向代理空闲断开（HTTP 上游路径同款）。
// 保活字节计入 openAIStreamKeepaliveBytesKey，不阻止切号。
//
// 返回的 stop 会等待保活 goroutine 退出；调用方必须在首次写下游、修改响应头
// 或读取 c.Writer 状态之前调用它，此后所有写入都只在调用方 goroutine 中进行。
func (s *OpenAIGatewayService) startOpenAIWSBridgeKeepalive(ctx context.Context, c *gin.Context, flusher http.Flusher) (stop func()) {
	interval := time.Duration(0)
	if s != nil && s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		interval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	if interval <= 0 || c == nil || c.Writer == nil || flusher == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := c.Writer.Write([]byte(":\n\n"))
				recordOpenAIStreamKeepaliveBytes(c, n)
				if err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

// guardOpenAIWSBridgeClientTurnState 对客户端回带的 x-codex-turn-state 做与 HTTP
// 上游路径相同的出站守卫：已知由其他账号铸造的值会被剥离。
func (s *OpenAIGatewayService) guardOpenAIWSBridgeClientTurnState(c *gin.Context, account *Account, turnState string) string {
	if strings.TrimSpace(turnState) == "" {
		return ""
	}
	probe := http.Header{}
	probe.Set(openAICodexTurnStateHeader, turnState)
	s.guardOpenAICodexTurnStateEcho(c, account, probe)
	return strings.TrimSpace(probe.Get(openAICodexTurnStateHeader))
}

// commitOpenAIWSBridgeStagedHeaders 在首个语义输出写下游前提交暂存的账号相关响应头。
// 响应头已被保活注释提交时整体丢弃（与 HTTP 路径一致）；无 turn-state 时清除
// writer 上可能残留的上一 failover attempt 的值。
func (s *OpenAIGatewayService) commitOpenAIWSBridgeStagedHeaders(c *gin.Context, account *Account, staged http.Header) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(staged)
	if state == "" {
		c.Writer.Header().Del(canonical)
		return
	}
	c.Writer.Header().Set(canonical, state)
	s.noteOpenAICodexTurnStateProvenance(c, account)
}

// clearOpenAIWSBridgeAttemptHeaders 清除本次 WS attempt 已设置但尚未提交的响应头，
// 避免切号或错误响应继承 SSE Content-Type 与上一个账号的 turn-state。
func clearOpenAIWSBridgeAttemptHeaders(c *gin.Context) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	h := c.Writer.Header()
	for _, key := range []string{"Content-Type", "Cache-Control", "Connection", "X-Accel-Buffering", openAICodexTurnStateHeader} {
		h.Del(key)
	}
}

// openAIWSBridgeStreamOutput 为 ws_bridge 的 HTTP 客户端重建终态事件的 output、
// 并对 error/response.failed 做客户端脱敏，与 HTTP 上游流式路径保持一致。
type openAIWSBridgeStreamOutput struct {
	accumulator  *apicompat.BufferedResponseAccumulator
	doneItems    *responsesStreamOutputItems
	imageOutputs []json.RawMessage
	seenImages   map[string]struct{}
}

func newOpenAIWSBridgeStreamOutput() *openAIWSBridgeStreamOutput {
	return &openAIWSBridgeStreamOutput{
		accumulator:  apicompat.NewBufferedResponseAccumulator(),
		doneItems:    newResponsesStreamOutputItems(),
		imageOutputs: make([]json.RawMessage, 0, 1),
		seenImages:   make(map[string]struct{}),
	}
}

// prepare 按事件到达顺序观察输出并返回要下发给客户端的事件。
func (o *openAIWSBridgeStreamOutput) prepare(eventType string, message []byte, clientOutputStarted bool) []byte {
	if o == nil {
		return message
	}
	if imageOutput, ok := extractImageGenerationOutputFromSSEData(message, o.seenImages); ok {
		o.imageOutputs = append(o.imageOutputs, imageOutput)
	}
	o.doneItems.Observe(message)
	if responsesStreamEventMayContributeToOutput(eventType) {
		var streamEvent apicompat.ResponsesStreamEvent
		if err := json.Unmarshal(message, &streamEvent); err == nil {
			o.accumulator.ProcessEvent(&streamEvent)
		}
	}
	if normalized, ok := normalizeResponsesStreamingTerminalOutput(message, o.accumulator, o.doneItems, o.imageOutputs); ok {
		message = normalized
	}
	if sanitized, ok := sanitizeOpenAIResponseFailedEventForClient(message, eventType, clientOutputStarted); ok {
		message = sanitized
	}
	return message
}

// newOpenAIWSBridgeEventFailoverError 对首个语义输出前的 error / response.failed 事件
// 按 HTTP 上游流式路径的规则判定是否切号。返回 nil 表示按原样下发给客户端。
// 账号副作用（限流、冷却、停用）由 WS 路径自身处理，这里不重复调用。
func (s *OpenAIGatewayService) newOpenAIWSBridgeEventFailoverError(
	c *gin.Context,
	account *Account,
	canonicalModel string,
	eventType string,
	payload []byte,
	headers http.Header,
) *UpstreamFailoverError {
	if s == nil || account == nil {
		return nil
	}
	message := extractOpenAISSEErrorMessage(payload)
	if message == "" {
		message = "Upstream response failed"
	}
	switch eventType {
	case "error":
		if !openAIStreamErrorEventShouldFailover(payload, message) {
			return nil
		}
	case "response.failed":
		if !openAIStreamFailedEventShouldFailover(payload, message) {
			return nil
		}
	default:
		return nil
	}
	statusCode := openAIStreamFailureStatus(payload, message)
	message = s.recordOpenAIStreamUpstreamError(c, account, false, "", "failover", payload, message)
	classificationHeaders := headers
	if statusCode == http.StatusTooManyRequests {
		// 流内 429 不能继承握手头里的全局配额快照（Spark OAuth 除外）。
		classificationHeaders = openAIWSSemantic429Headers(account, canonicalModel, headers)
	}
	return s.newOpenAIAccountFailoverErrorWithClassificationHeaders(
		account,
		statusCode,
		headers,
		classificationHeaders,
		payload,
		message,
		false,
		openAIStreamFailedEventRetryableOnSameAccount(account, payload, message),
	)
}

// newOpenAIWSBridgeFallbackFailoverError 把 WS 重试用尽后的失败映射为切号错误，
// 对齐 HTTP 上游路径（传输错误与 5xx/401/429 均换号）。返回 nil 表示沿用原有处理：
// 已写出语义输出、客户端已断开、或属于请求级错误（invalid_encrypted_content /
// previous_response_not_found 仍以 400 返回客户端）。
func (s *OpenAIGatewayService) newOpenAIWSBridgeFallbackFailoverError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	canonicalModel string,
	wsErr error,
) *UpstreamFailoverError {
	if s == nil || account == nil || wsErr == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	if openAIStreamClientOutputStarted(c, false) {
		return nil
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(wsErr, &fallbackErr) || fallbackErr == nil {
		return nil
	}
	switch strings.TrimPrefix(strings.TrimSpace(fallbackErr.Reason), "prewarm_") {
	case "", "invalid_encrypted_content", "previous_response_not_found", "retry_backoff_canceled":
		return nil
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(wsErr.Error()))
	failoverErr := &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: openAITransportFailoverBody,
	}
	var dialErr *openAIWSDialError
	if errors.As(fallbackErr.Err, &dialErr) && dialErr != nil {
		switch statusCode := dialErr.StatusCode; {
		case statusCode == http.StatusTooManyRequests:
			// forwardOpenAIWSV2 已持久化拨号 429 的限流信号。
			failoverErr = s.newOpenAIWSRateLimitFailoverError(account, dialErr.ResponseHeaders, nil, message)
		case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
			shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, statusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, canonicalModel)
			failoverErr = s.newOpenAIAccountFailoverError(account, statusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, message, shouldDisable, false)
		case statusCode >= http.StatusInternalServerError:
			// 冷却已由 handleOpenAIWSDialTransientFailure 处理。
			failoverErr = s.newOpenAIAccountFailoverError(account, statusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, message, false, false)
		}
	}
	if c != nil {
		proxyID, proxyName := opsUpstreamWSProxyAttribution(account)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            proxyID,
			ProxyName:          proxyName,
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: failoverErr.StatusCode,
			Kind:               "failover",
			Message:            message,
		})
	}
	return failoverErr
}

// openAIWSBridgeSameAccountRetryable 判定 ws_bridge HTTP 请求的 WS 失败是否值得在同一
// 账号上重试。只有连接级的读写失败（多为池中空闲连接已被上游关闭）重试一次；
// 5xx、拨号失败、上游错误事件等按 HTTP 上游路径立即交给 handler 切号。
func openAIWSBridgeSameAccountRetryable(reason string, attempt int) bool {
	if attempt > 1 {
		return false
	}
	switch strings.TrimPrefix(strings.TrimSpace(reason), "prewarm_") {
	case "read_event", "write_request", "write":
		return true
	default:
		return false
	}
}

// openAIWSBridgeClientErrorPayload 返回可以写回客户端的 error 事件与 message：先按
// HTTP 上游路径做 agent identity 凭据脱敏，再提取（已清洗的）错误信息。切号判定、
// 账号副作用与日志仍使用原始 payload。
func (s *OpenAIGatewayService) openAIWSBridgeClientErrorPayload(ctx context.Context, account *Account, payload []byte) ([]byte, string) {
	redacted := s.redactAgentIdentitySensitiveBody(ctx, account, payload)
	return redacted, extractOpenAISSEErrorMessage(redacted)
}

// writeOpenAIWSBridgeUncommittedErrorEvent 在尚未向客户端写出任何内容时，按 HTTP 上游
// 返回非 2xx 的语义写回 error 事件：错误透传规则优先，确定性请求错误（400）保留
// type/code/param，其余按 handleErrorResponse 的通用映射。payload 与 clientMsg 须已经
// openAIWSBridgeClientErrorPayload 脱敏；调用方需已停止保活。
func (s *OpenAIGatewayService) writeOpenAIWSBridgeUncommittedErrorEvent(c *gin.Context, account *Account, payload []byte, clientMsg string, statusCode int) {
	if c == nil || c.Writer == nil || c.Writer.Written() || account == nil {
		return
	}
	clearOpenAIWSBridgeAttemptHeaders(c)
	MarkResponseCommitted(c)
	if status, errType, ruleMsg, matched := applyOpenAIStreamFailedErrorPassthroughRule(c, account.Platform, payload, clientMsg); matched {
		c.JSON(status, gin.H{"error": gin.H{"type": errType, "message": ruleMsg}})
		return
	}
	if isOpenAIDeterministicClientError(statusCode) {
		writeOpenAIUpstreamClientError(c, statusCode, payload, clientMsg)
		return
	}
	status, errType, genericMsg := openAIUpstreamGenericClientError(statusCode, clientMsg, payload)
	c.JSON(status, gin.H{"error": gin.H{"type": errType, "message": genericMsg}})
}

// openAIWSBridgeCommittedErrorFrames 返回响应已提交（保活或已有输出）后 error 事件要依次
// 下发的 data 帧，保证恰好一个终止事件：WS 路径收到 error 即结束，等不到上游随后的
// response.failed，因此总是补一个带真实 code/message 的 response.failed（降载码改写为
// 可重试的 server_error）。OAuth 类账号不发裸 error 帧（对齐 HTTP 路径对 Codex 的处理）；
// API Key 账号先转发经脱敏的 error 帧。payload 须已经 openAIWSBridgeClientErrorPayload 脱敏。
func openAIWSBridgeCommittedErrorFrames(account *Account, responseID, model string, payload []byte, clientMsg string, output *openAIWSBridgeStreamOutput, clientOutputStarted bool) [][]byte {
	failedSource, _ := sanitizeOpenAICapacityShedErrorCodeForClient(payload)
	// createdAt=0：桥接路径不跟踪 created_at，由构造函数取当前时间（严格客户端要求必带）。
	failed := buildOpenAIResponseFailedPayload(responseID, model, 0, failedSource, clientMsg)
	if account != nil && account.IsOpenAIOAuthLike() {
		return [][]byte{failed}
	}
	return [][]byte{output.prepare("error", payload, clientOutputStarted), failed}
}

// writeOpenAIWSBridgeCommittedFallbackError 处理保活已提交 200、但尚无语义输出时的
// WS 降级错误（如 previous_response_not_found）：把真实原因写成 response.failed，
// 避免 writeOpenAIWSFallbackErrorResponse 因 Written() 跳过、最终只剩通用错误。
// 返回 false 表示不适用（未写出、已有语义输出或无法归类），由调用方沿用原有处理。
func (s *OpenAIGatewayService) writeOpenAIWSBridgeCommittedFallbackError(c *gin.Context, account *Account, model string, wsErr error) bool {
	if c == nil || c.Writer == nil || !c.Writer.Written() || openAIStreamClientOutputStarted(c, false) {
		return false
	}
	statusCode, errType, clientMessage, upstreamMessage, ok := resolveOpenAIWSFallbackErrorResponse(wsErr)
	if !ok {
		return false
	}
	code := "upstream_error"
	var fallbackErr *openAIWSFallbackError
	if errors.As(wsErr, &fallbackErr) && fallbackErr != nil {
		switch reason := strings.TrimPrefix(strings.TrimSpace(fallbackErr.Reason), "prewarm_"); reason {
		case "previous_response_not_found", "invalid_encrypted_content":
			code = reason
		}
	}
	if strings.TrimSpace(clientMessage) == "" {
		clientMessage = "Upstream request failed"
	}
	if strings.TrimSpace(upstreamMessage) == "" {
		upstreamMessage = clientMessage
	}
	source, err := json.Marshal(gin.H{"error": gin.H{"type": errType, "code": code, "message": clientMessage}})
	if err != nil {
		return false
	}
	setOpsUpstreamError(c, statusCode, upstreamMessage, "")
	if account != nil {
		proxyID, proxyName := opsUpstreamWSProxyAttribution(account)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            proxyID,
			ProxyName:          proxyName,
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: statusCode,
			Kind:               "ws_error",
			Message:            upstreamMessage,
		})
	}
	MarkResponseCommitted(c)
	if _, err := c.Writer.WriteString(buildOpenAIResponseFailedSSE("", model, 0, source, clientMessage)); err == nil {
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return true
}
