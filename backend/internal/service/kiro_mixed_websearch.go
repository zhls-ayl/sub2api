package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const kiroWebSearchLoopTimeout = 4 * time.Minute

// errKiroWebSearchToolFailed stops the loop after a search has been surfaced
// to the client as web_search_tool_result_error. It is not a hard failure.
var errKiroWebSearchToolFailed = errors.New("kiro web search tool failed")

type kiroObservedToolUse struct {
	Index int
	ID    string
	Name  string
	Query string
}

type kiroModelRound struct {
	HTTPErr    *kiroWebSearchHTTPError
	Chunks     [][]byte
	Stream     *kiropkg.StreamResult
	Body       []byte
	Usage      kiropkg.Usage
	StopReason string
	RequestID  string
}

type kiroRoundClient interface {
	complete(ctx context.Context, body []byte, stream bool, iteration int) (*kiroModelRound, error)
	search(ctx context.Context, query string) (*kiropkg.WebSearchResults, error)
}

type gatewayKiroRoundClient struct {
	svc          *GatewayService
	account      *Account
	group        *Group
	token        string
	mappedModel  string
	requestModel string
	headers      http.Header
	inputTokens  int
	originalBody []byte
	plan         *kiroCacheEmulationPlan
	cacheUsage   *kiroCacheEmulationUsage
	cacheReady   bool
	upstream     func(ctx context.Context, body []byte) (*http.Response, error)
	commitCache  func()
	// enhanced selects mixed-tool loop behavior. With it off, a later-round
	// upstream failure is returned unchanged so the only-web_search path
	// matches the pre-switch gateway.
	enhanced bool
}

func (s *GatewayService) newGatewayKiroRoundClient(account *Account, group *Group, token, mappedModel, requestModel string, headers http.Header, inputTokens int, originalBody []byte, plan *kiroCacheEmulationPlan) *gatewayKiroRoundClient {
	return &gatewayKiroRoundClient{
		svc:          s,
		account:      account,
		group:        group,
		token:        token,
		mappedModel:  mappedModel,
		requestModel: requestModel,
		headers:      headers,
		inputTokens:  inputTokens,
		originalBody: originalBody,
		plan:         plan,
	}
}

func (c *gatewayKiroRoundClient) search(ctx context.Context, query string) (*kiropkg.WebSearchResults, error) {
	if c == nil || c.svc == nil {
		return nil, fmt.Errorf("kiro web search client is not configured")
	}
	results, nextToken, err := c.svc.callKiroWebSearchMCP(ctx, c.account, c.token, query)
	if strings.TrimSpace(nextToken) != "" {
		c.token = nextToken
	}
	return results, err
}

func (c *gatewayKiroRoundClient) complete(ctx context.Context, body []byte, stream bool, iteration int) (*kiroModelRound, error) {
	if c == nil || (c.svc == nil && c.upstream == nil) {
		return nil, fmt.Errorf("kiro web search client is not configured")
	}
	var resp *http.Response
	var err error
	if c.upstream != nil {
		resp, err = c.upstream(ctx, body)
	} else {
		resp, _, err = c.svc.executeKiroUpstream(ctx, c.account, body, c.mappedModel, c.requestModel, c.token, c.headers)
	}
	if err != nil {
		if iteration > 0 && c.enhanced {
			return nil, kiroWebSearchLaterRoundError(err)
		}
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("kiro web search upstream returned nil response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpErr := &kiroWebSearchHTTPError{Response: resp}
		if iteration > 0 && c.enhanced {
			_ = resp.Body.Close()
			return nil, kiroWebSearchLaterRoundError(httpErr)
		}
		return &kiroModelRound{HTTPErr: httpErr}, nil
	}
	if !c.cacheReady {
		if c.commitCache != nil {
			c.commitCache()
		} else if stream {
			if c.plan != nil {
				c.plan.commit()
				c.cacheUsage = c.plan.result()
			}
		} else if c.svc != nil {
			c.cacheUsage = c.svc.buildKiroCacheEmulationUsage(ctx, c.account, c.group, c.originalBody, c.mappedModel, c.inputTokens)
		}
		c.cacheReady = true
	}
	defer func() { _ = resp.Body.Close() }()
	requestID := buildKiroRequestID(resp)
	if stream {
		chunks, result, streamErr := bufferKiroAnthropicStream(ctx, resp.Body, c.requestModel, c.inputTokens)
		if streamErr != nil {
			return nil, streamErr
		}
		return &kiroModelRound{Chunks: chunks, Stream: result, RequestID: requestID}, nil
	}
	parsed, parseErr := kiropkg.ParseNonStreamingEventStreamWithContext(resp.Body, c.requestModel, kiropkg.KiroRequestContext{
		CacheEmulationUsage:  c.cacheUsage.toKiroUsage(),
		EstimatedInputTokens: c.inputTokens,
		TokenizerAlgorithm:   c.svc.kiroMeteringAlgorithm(ctx),
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if parsed == nil {
		return nil, fmt.Errorf("kiro web search parsed empty response")
	}
	return &kiroModelRound{
		Body:       parsed.ResponseBody,
		Usage:      parsed.Usage,
		StopReason: gjson.GetBytes(parsed.ResponseBody, "stop_reason").String(),
		RequestID:  requestID,
	}, nil
}

func (c *gatewayKiroRoundClient) prefetch(ctx context.Context) {
	if c == nil || c.svc == nil {
		return
	}
	c.svc.prefetchKiroWebSearchDescription(ctx, c.account, c.token)
}

// kiroWebSearchLoopContext applies the mixed-tool loop deadline only when that
// behavior is enabled. The switch-off only-web_search path keeps the caller ctx.
func kiroWebSearchLoopContext(ctx context.Context, enhanced bool) (context.Context, context.CancelFunc) {
	if !enhanced {
		if ctx == nil {
			ctx = context.Background()
		}
		return ctx, func() {}
	}
	return withKiroWebSearchLoopDeadline(ctx)
}

func withKiroWebSearchLoopDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, kiroWebSearchLoopTimeout)
}

func kiroWebSearchHardError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		return err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "context canceled") || strings.Contains(msg, "context deadline exceeded") {
		return err
	}
	return nil
}

func kiroWebSearchToolErrorCode(err error) string {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "status 429") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "too_many_requests") || strings.Contains(msg, "rate limit") {
		return "too_many_requests"
	}
	return "unavailable"
}

// kiroWebSearchLaterRoundFailure marks an upstream failure after the first
// model round has already succeeded. Unwrap exposes the cause to errors.As.
// Callers must handle this wrapper before *kiroWebSearchHTTPError or
// *UpstreamFailoverError, or a started loop would be failed over.
type kiroWebSearchLaterRoundFailure struct {
	err error
}

func (e *kiroWebSearchLaterRoundFailure) Error() string {
	if e == nil || e.err == nil {
		return "kiro web search upstream failed after first round"
	}
	return "kiro web search upstream failed after first round: " + e.err.Error()
}

func (e *kiroWebSearchLaterRoundFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func kiroWebSearchLaterRoundError(err error) error {
	if err == nil {
		err = errors.New("kiro web search upstream failed after first round")
	}
	return &kiroWebSearchLaterRoundFailure{err: err}
}

func kiroWebSearchIsLaterRound(err error) bool {
	var later *kiroWebSearchLaterRoundFailure
	return errors.As(err, &later)
}

// kiroWebSearchNonStreamFailureAction is the forward-layer decision for a
// non-streaming web-search loop error. A later-round wrapper is a plain 502
// even when it unwraps to an HTTP or failover error.
func kiroWebSearchNonStreamFailureAction(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errKiroWebSearchFallback):
		return "fallback"
	case kiroWebSearchIsLaterRound(err):
		return "later_round"
	default:
		var httpErr *kiroWebSearchHTTPError
		if errors.As(err, &httpErr) && httpErr.Response != nil {
			return "failover_http"
		}
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			return "failover"
		}
		return "bad_gateway"
	}
}

func mergeKiroRoundUsage(dst *kiropkg.Usage, src kiropkg.Usage) {
	if dst == nil {
		return
	}
	dst.OutputTokens += src.OutputTokens
	dst.KiroCredits += src.KiroCredits
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.CacheReadInputTokens > 0 {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheCreationInputTokens > 0 {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheCreation5mInputTokens > 0 {
		dst.CacheCreation5mInputTokens = src.CacheCreation5mInputTokens
	}
	if src.CacheCreation1hInputTokens > 0 {
		dst.CacheCreation1hInputTokens = src.CacheCreation1hInputTokens
	}
}

func absorbKiroStreamRound(dst *kiropkg.Usage, stop *string, result *kiropkg.StreamResult) {
	if result == nil {
		return
	}
	mergeKiroRoundUsage(dst, result.Usage)
	if stop != nil && strings.TrimSpace(result.StopReason) != "" {
		*stop = strings.TrimSpace(result.StopReason)
	}
}

// dropExecutedWebSearchToolUses removes web_search tool_use blocks that the
// loop already surfaced as server-tool indicators, and keeps the model's text.
func dropExecutedWebSearchToolUses(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return body
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return body
	}
	kept := make([]any, 0, len(content))
	removed := false
	for _, block := range content {
		item, ok := block.(map[string]any)
		if !ok {
			kept = append(kept, block)
			continue
		}
		if getInterfaceString(item["type"]) == "tool_use" && kiropkg.IsWebSearchToolName(getInterfaceString(item["name"]), "") {
			removed = true
			continue
		}
		kept = append(kept, block)
	}
	if !removed {
		return body
	}
	msg["content"] = kept
	msg["stop_reason"] = "end_turn"
	encoded, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return encoded
}

func kiroWebSearchClientMessage(model string, usage ClaudeUsage, searches []kiropkg.SearchIndicator, modelBody []byte) []byte {
	body := modelBody
	if len(body) == 0 {
		if strings.TrimSpace(model) == "" {
			model = "kiro"
		}
		payload, err := json.Marshal(map[string]any{
			"id":            "msg_" + kiropkg.GenerateToolUseID(),
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"content":       []any{},
			"usage":         map[string]any{},
		})
		if err != nil {
			return nil
		}
		body = payload
	}
	if injected, err := kiropkg.InjectSearchIndicatorsInResponse(body, searches); err == nil {
		body = injected
	}
	body = kiropkg.RenameRemoteWebSearchToolUses(body)
	return applyKiroWebSearchClientUsage(body, usage, len(searches))
}

func applyKiroWebSearchClientUsage(body []byte, usage ClaudeUsage, calls int) []byte {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return body
	}
	usageObj, _ := msg["usage"].(map[string]any)
	if usageObj == nil {
		usageObj = map[string]any{}
	}
	usageObj["input_tokens"] = usage.InputTokens
	usageObj["output_tokens"] = usage.OutputTokens
	if usage.CacheCreationInputTokens > 0 || usage.CacheReadInputTokens > 0 {
		usageObj["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
		usageObj["cache_read_input_tokens"] = usage.CacheReadInputTokens
	}
	if calls > 0 {
		usageObj["server_tool_use"] = map[string]any{"web_search_requests": calls}
	}
	msg["usage"] = usageObj
	encoded, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return encoded
}

func hasMixedKiroWebSearchTools(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	arr := tools.Array()
	if len(arr) < 2 {
		return false
	}
	for _, tool := range arr {
		if isWebSearchToolJSON(tool) || kiropkg.IsWebSearchToolName(tool.Get("name").String(), tool.Get("type").String()) {
			return true
		}
	}
	return false
}

func kiroTurnContinuesWebSearch(tools []kiroObservedToolUse, iteration int) bool {
	if iteration+1 >= kiroMaxWebSearchIterations || len(tools) == 0 {
		return false
	}
	for _, tool := range tools {
		if !kiropkg.IsWebSearchToolName(tool.Name, "") || strings.TrimSpace(tool.Query) == "" {
			return false
		}
	}
	return true
}

func observeClaudeToolUses(body []byte) []kiroObservedToolUse {
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return nil
	}
	tools := make([]kiroObservedToolUse, 0)
	for i, block := range content.Array() {
		if block.Get("type").String() != "tool_use" {
			continue
		}
		tools = append(tools, kiroObservedToolUse{
			Index: i,
			ID:    block.Get("id").String(),
			Name:  block.Get("name").String(),
			Query: strings.TrimSpace(block.Get("input.query").String()),
		})
	}
	return tools
}

func observeBufferedToolUses(chunks [][]byte) []kiroObservedToolUse {
	tools := make([]kiroObservedToolUse, 0)
	var (
		active       bool
		currentName  string
		currentID    string
		currentIndex int
		input        strings.Builder
	)
	flush := func() {
		if !active {
			return
		}
		query := ""
		var parsed map[string]any
		if err := json.Unmarshal([]byte(input.String()), &parsed); err == nil && parsed["query"] != nil {
			query = strings.TrimSpace(fmt.Sprint(parsed["query"]))
		}
		tools = append(tools, kiroObservedToolUse{
			Index: currentIndex,
			ID:    strings.TrimSpace(currentID),
			Name:  currentName,
			Query: query,
		})
		active = false
		currentName = ""
		currentID = ""
		input.Reset()
	}
	for _, chunk := range chunks {
		for _, line := range strings.Split(string(chunk), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				continue
			}
			switch eventType, _ := event["type"].(string); eventType {
			case "content_block_start":
				flush()
				block, _ := event["content_block"].(map[string]any)
				if block == nil || getInterfaceString(block["type"]) != "tool_use" {
					continue
				}
				active = true
				currentName = getInterfaceString(block["name"])
				currentID = getInterfaceString(block["id"])
				if idx, ok := event["index"].(float64); ok {
					currentIndex = int(idx)
				}
				input.Reset()
			case "content_block_delta":
				if !active {
					continue
				}
				delta, _ := event["delta"].(map[string]any)
				if delta == nil || getInterfaceString(delta["type"]) != "input_json_delta" {
					continue
				}
				if partial, ok := delta["partial_json"].(string); ok {
					_, _ = input.WriteString(partial)
				}
			case "content_block_stop":
				flush()
			}
		}
	}
	flush()
	return tools
}

func getInterfaceString(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	default:
		if v == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(val))
	}
}

func executePureWebSearchTurn(ctx context.Context, client kiroRoundClient, tools []kiroObservedToolUse) ([]kiropkg.SearchIndicator, error) {
	searches := make([]kiropkg.SearchIndicator, 0, len(tools))
	for _, tool := range tools {
		if err := ctx.Err(); err != nil {
			return searches, err
		}
		id := strings.TrimSpace(tool.ID)
		if id == "" {
			id = "srvtoolu_" + kiropkg.GenerateToolUseID()
		}
		results, mcpErr := client.search(ctx, tool.Query)
		if mcpErr != nil {
			if hard := kiroWebSearchHardError(mcpErr); hard != nil {
				return searches, hard
			}
			code := kiroWebSearchToolErrorCode(mcpErr)
			logger.L().Warn("kiro web search mcp failed",
				zap.String("error_code", code),
				zap.String("query", tool.Query),
				zap.Error(mcpErr),
			)
			searches = append(searches, kiropkg.SearchIndicator{
				ToolUseID: id,
				Query:     tool.Query,
				ErrorCode: code,
			})
			return searches, errKiroWebSearchToolFailed
		}
		searches = append(searches, kiropkg.SearchIndicator{
			ToolUseID: id,
			Query:     tool.Query,
			Results:   results,
		})
	}
	return searches, nil
}

func writeKiroSearchIndicators(w io.Writer, searches []kiropkg.SearchIndicator, startIndex int) (int, error) {
	index := startIndex
	for _, search := range searches {
		var chunks [][]byte
		if strings.TrimSpace(search.ErrorCode) != "" {
			chunks = kiropkg.GenerateSearchIndicatorErrorEvents(search.Query, search.ToolUseID, search.ErrorCode, index)
		} else {
			chunks = kiropkg.GenerateSearchIndicatorEvents(search.Query, search.ToolUseID, search.Results, index)
		}
		if err := writeSSEChunks(w, chunks); err != nil {
			return index, err
		}
		index += 2
	}
	return index, nil
}

func runKiroMixedNonStream(ctx context.Context, client kiroRoundClient, body []byte, requestModel string, inputTokens int) (*kiroWebSearchExecution, error) {
	current := body
	var accUsage kiropkg.Usage
	searches := make([]kiropkg.SearchIndicator, 0, 2)
	requestID := ""
	for iteration := 0; iteration < kiroMaxWebSearchIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		round, err := client.complete(ctx, current, false, iteration)
		if err != nil {
			return nil, err
		}
		if round == nil {
			return nil, fmt.Errorf("kiro web search empty round")
		}
		if round.HTTPErr != nil {
			return nil, round.HTTPErr
		}
		if requestID == "" {
			requestID = round.RequestID
		}
		mergeKiroRoundUsage(&accUsage, round.Usage)
		tools := observeClaudeToolUses(round.Body)
		if !kiroTurnContinuesWebSearch(tools, iteration) {
			usage := kiroUsageToClaude(accUsage, inputTokens)
			return &kiroWebSearchExecution{
				ResponseBody:   kiroWebSearchClientMessage(requestModel, usage, searches, round.Body),
				Usage:          usage,
				RequestID:      requestID,
				WebSearchCalls: len(searches),
			}, nil
		}
		turn, searchErr := executePureWebSearchTurn(ctx, client, tools)
		searches = append(searches, turn...)
		if errors.Is(searchErr, errKiroWebSearchToolFailed) {
			usage := kiroUsageToClaude(accUsage, inputTokens)
			return &kiroWebSearchExecution{
				ResponseBody:   kiroWebSearchClientMessage(requestModel, usage, searches, dropExecutedWebSearchToolUses(round.Body)),
				Usage:          usage,
				RequestID:      requestID,
				WebSearchCalls: len(searches),
			}, nil
		}
		if searchErr != nil {
			return nil, searchErr
		}
		nextBody, injectErr := kiropkg.InjectWebSearchToolResults(current, turn)
		if injectErr != nil {
			return nil, injectErr
		}
		current = nextBody
	}
	return nil, fmt.Errorf("kiro web search exceeded max iterations")
}

func runKiroMixedStream(ctx context.Context, client kiroRoundClient, w io.Writer, body []byte, requestModel string, inputTokens int, cacheUsage *kiroCacheEmulationUsage) error {
	round, err := client.complete(ctx, body, true, 0)
	if err != nil {
		return err
	}
	if round != nil && round.HTTPErr != nil {
		return round.HTTPErr
	}
	if err := writeAnthropicMessageStart(w, "", requestModel, inputTokens, cacheUsage); err != nil {
		return err
	}
	return continueKiroMixedStream(ctx, client, w, body, round, inputTokens)
}

func continueKiroMixedStream(ctx context.Context, client kiroRoundClient, w io.Writer, body []byte, first *kiroModelRound, inputTokens int) error {
	current := body
	var accUsage kiropkg.Usage
	accStop := ""
	searches := make([]kiropkg.SearchIndicator, 0, 2)
	nextIndex := 0
	round := first
	for iteration := 0; iteration < kiroMaxWebSearchIterations; iteration++ {
		if iteration > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			next, err := client.complete(ctx, current, true, iteration)
			if err != nil {
				status := 0
				var httpErr *kiroWebSearchHTTPError
				if errors.As(err, &httpErr) && httpErr.Response != nil {
					status = httpErr.Response.StatusCode
				}
				logger.L().Warn("kiro mixed web search upstream failed after first round",
					zap.Int("iteration", iteration),
					zap.Int("status", status),
					zap.Error(err),
				)
				return writeKiroWebSearchStreamTermination(w, &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}, len(searches))
			}
			round = next
		}
		if round == nil {
			return fmt.Errorf("kiro web search empty round")
		}
		if round.HTTPErr != nil {
			if iteration == 0 {
				return round.HTTPErr
			}
			status := 0
			if round.HTTPErr.Response != nil {
				status = round.HTTPErr.Response.StatusCode
			}
			logger.L().Warn("kiro mixed web search upstream status failed after first round",
				zap.Int("iteration", iteration),
				zap.Int("status", status),
			)
			return writeKiroWebSearchStreamTermination(w, &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}, len(searches))
		}
		absorbKiroStreamRound(&accUsage, &accStop, round.Stream)
		if accUsage.InputTokens == 0 && inputTokens > 0 {
			accUsage.InputTokens = inputTokens
		}
		tools := observeBufferedToolUses(round.Chunks)
		final := &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}
		if !kiroTurnContinuesWebSearch(tools, iteration) {
			return writeKiroWebSearchFinalRound(w, round.Chunks, nextIndex, final, len(searches), true)
		}
		indices := make([]int, 0, len(tools))
		for _, tool := range tools {
			indices = append(indices, tool.Index)
		}
		filtered := kiropkg.FilterChunksDroppingIndices(round.Chunks, indices, nextIndex)
		if err := writeSSEChunks(w, filtered); err != nil {
			return err
		}
		if maxIndex := kiropkg.MaxContentBlockIndex(filtered); maxIndex >= nextIndex {
			nextIndex = maxIndex + 1
		}
		turn, searchErr := executePureWebSearchTurn(ctx, client, tools)
		var writeErr error
		nextIndex, writeErr = writeKiroSearchIndicators(w, turn, nextIndex)
		if writeErr != nil {
			return writeErr
		}
		searches = append(searches, turn...)
		if errors.Is(searchErr, errKiroWebSearchToolFailed) {
			return writeKiroWebSearchStreamTermination(w, &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}, len(searches))
		}
		if searchErr != nil {
			return searchErr
		}
		nextBody, injectErr := kiropkg.InjectWebSearchToolResults(current, turn)
		if injectErr != nil {
			return injectErr
		}
		current = nextBody
		round = nil
	}
	return fmt.Errorf("kiro web search exceeded max iterations")
}

func (s *GatewayService) executeKiroMixedToolWebSearch(ctx context.Context, account *Account, group *Group, anthropicBody []byte, mappedModel, requestModel, token string, headers http.Header) (*kiroWebSearchExecution, error) {
	loopCtx, cancel := withKiroWebSearchLoopDeadline(ctx)
	defer cancel()
	inputTokens := estimateKiroInputTokens(loopCtx, s.kiroMeteringAlgorithm(loopCtx), anthropicBody)
	client := s.newGatewayKiroRoundClient(account, group, token, mappedModel, requestModel, headers, inputTokens, anthropicBody, nil)
	client.enhanced = true
	return runKiroMixedNonStream(loopCtx, client, anthropicBody, requestModel, inputTokens)
}

func (s *GatewayService) openKiroMixedToolWebSearchStream(ctx context.Context, account *Account, anthropicBody []byte, mappedModel, requestModel, token string, headers http.Header, inputTokens int, plan *kiroCacheEmulationPlan) (*http.Response, error) {
	loopCtx, cancel := withKiroWebSearchLoopDeadline(ctx)
	client := s.newGatewayKiroRoundClient(account, nil, token, mappedModel, requestModel, headers, inputTokens, anthropicBody, plan)
	client.enhanced = true
	first, err := client.complete(loopCtx, anthropicBody, true, 0)
	if err != nil {
		cancel()
		return nil, err
	}
	if first != nil && first.HTTPErr != nil {
		cancel()
		return first.HTTPErr.Response, nil
	}
	pr, pw := io.Pipe()
	outHeaders := make(http.Header)
	outHeaders.Set("Content-Type", "text/event-stream")
	go func() {
		defer cancel()
		if err := writeAnthropicMessageStart(pw, "", requestModel, inputTokens, plan.result()); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		streamErr := continueKiroMixedStream(loopCtx, client, pw, anthropicBody, first, inputTokens)
		if streamErr != nil {
			_ = pw.CloseWithError(streamErr)
			return
		}
		_ = pw.Close()
	}()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     outHeaders,
		Body:       pr,
	}, nil
}
