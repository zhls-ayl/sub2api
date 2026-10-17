package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const kiroMaxWebSearchIterations = 5

var (
	errKiroWebSearchFallback = errors.New("kiro web search fallback")
	kiroWebSearchDescCache   sync.Map
)

type kiroWebSearchExecution struct {
	ResponseBody   []byte
	Usage          ClaudeUsage
	RequestID      string
	WebSearchCalls int
}

type kiroWebSearchHTTPError struct {
	Response *http.Response
}

type kiroStreamChunkCollector struct {
	chunks [][]byte
}

func (e *kiroWebSearchHTTPError) Error() string {
	if e == nil || e.Response == nil {
		return "kiro web search http error"
	}
	return fmt.Sprintf("kiro web search http error: %d", e.Response.StatusCode)
}

func (w *kiroStreamChunkCollector) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.chunks = append(w.chunks, append([]byte(nil), p...))
	}
	return len(p), nil
}

func bufferKiroAnthropicStream(ctx context.Context, body io.Reader, responseModel string, inputTokens int) ([][]byte, *kiropkg.StreamResult, error) {
	collector := &kiroStreamChunkCollector{}
	result, err := kiropkg.StreamEventStreamAsAnthropicWithContext(ctx, body, collector, responseModel, inputTokens, kiropkg.KiroRequestContext{})
	if err != nil {
		return nil, nil, err
	}
	return collector.chunks, result, nil
}

// writeKiroWebSearchFinalRound forwards the last upstream turn's content blocks
// and then closes the SSE stream. AdjustSSEChunk intentionally drops
// message_start/message_delta/message_stop; without a replacement message_delta
// and message_stop, handleStreamingResponse reports "missing terminal event"
// and the request is not billed.
func writeKiroWebSearchFinalRound(w io.Writer, chunks [][]byte, indexOffset int, result *kiropkg.StreamResult, webSearchRequests int, renameRemoteWebSearch bool) error {
	for _, chunk := range chunks {
		adjusted, shouldForward := kiropkg.AdjustSSEChunk(chunk, indexOffset)
		if !shouldForward {
			continue
		}
		if renameRemoteWebSearch {
			adjusted = kiropkg.RenameRemoteWebSearchInSSEChunk(adjusted)
		}
		if _, err := w.Write(adjusted); err != nil {
			return err
		}
	}
	return writeKiroWebSearchStreamTermination(w, result, webSearchRequests)
}

// writeKiroWebSearchStreamTermination emits the client-visible end of a Kiro
// web-search stream. output_tokens and credits come from the parsed upstream
// turn (summed across search rounds). input_tokens are omitted so the
// cache-adjusted values already sent in message_start are not replaced by the
// raw upstream count.
func writeKiroWebSearchStreamTermination(w io.Writer, result *kiropkg.StreamResult, webSearchRequests int) error {
	stopReason := "end_turn"
	outputTokens := 0
	credits := 0.0
	if result != nil {
		if reason := strings.TrimSpace(result.StopReason); reason != "" {
			stopReason = reason
		}
		outputTokens = result.Usage.OutputTokens
		credits = result.Usage.KiroCredits
	}
	usage := map[string]any{
		"output_tokens": outputTokens,
	}
	if webSearchRequests > 0 {
		usage["server_tool_use"] = map[string]any{
			"web_search_requests": webSearchRequests,
		}
	}
	if credits > 0 {
		usage["_sub2api_kiro_credits"] = credits
	}
	delta, err := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": usage,
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: message_delta\ndata: "+string(delta)+"\n\n"); err != nil {
		return err
	}
	stop, err := json.Marshal(map[string]any{"type": "message_stop"})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "event: message_stop\ndata: "+string(stop)+"\n\n")
	return err
}

func writeSSEChunks(w io.Writer, chunks [][]byte) error {
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := w.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func writeAnthropicMessageStart(w io.Writer, msgID, model string, inputTokens int, cacheUsage *kiroCacheEmulationUsage) error {
	if strings.TrimSpace(msgID) == "" {
		msgID = "msg_" + kiropkg.GenerateToolUseID()
	}
	if strings.TrimSpace(model) == "" {
		model = "kiro"
	}
	usage := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": 0,
	}
	if cacheUsage != nil {
		usage["input_tokens"] = cacheUsage.InputTokens
		usage["cache_creation_input_tokens"] = cacheUsage.CacheCreationInputTokens
		usage["cache_read_input_tokens"] = cacheUsage.CacheReadInputTokens
	}
	payload, err := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usage,
		},
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "event: message_start\ndata: "+string(payload)+"\n\n")
	return err
}

func (s *GatewayService) streamKiroWebSearchAsAnthropic(
	ctx context.Context, account *Account, anthropicBody []byte, mappedModel, requestModel, token string, inputTokens int, headers http.Header, w io.Writer, plan *kiroCacheEmulationPlan, enhanced bool,
) error {
	client := s.newGatewayKiroRoundClient(account, nil, token, mappedModel, requestModel, headers, inputTokens, anthropicBody, plan)
	client.enhanced = enhanced
	var cacheUsage *kiroCacheEmulationUsage
	if plan != nil {
		cacheUsage = plan.result()
	}
	return runKiroOnlyWebSearchStream(ctx, client, client.prefetch, anthropicBody, requestModel, inputTokens, w, cacheUsage, enhanced)
}

func (s *GatewayService) executeKiroWebSearch(ctx context.Context, account *Account, group *Group, anthropicBody []byte, mappedModel, requestModel, token string, headers http.Header, enhanced bool) (*kiroWebSearchExecution, error) {
	inputTokens := estimateKiroInputTokens(ctx, s.kiroMeteringAlgorithm(ctx), anthropicBody)
	client := s.newGatewayKiroRoundClient(account, group, token, mappedModel, requestModel, headers, inputTokens, anthropicBody, nil)
	client.enhanced = enhanced
	return runKiroOnlyWebSearchNonStream(ctx, client, client.prefetch, anthropicBody, requestModel, inputTokens, enhanced)
}

// runKiroOnlyWebSearchStream is the only-web_search streaming loop.
// enhanced gates behavior added for the mixed-tool switch: MCP failures become
// tool_result errors, rounds share a deadline, output and credits accumulate,
// and web_search calls are counted. With enhanced off, the loop matches the
// original only-web_search stream plus the terminal message_delta/message_stop.
func runKiroOnlyWebSearchStream(
	ctx context.Context,
	client kiroRoundClient,
	prefetch func(context.Context),
	anthropicBody []byte,
	requestModel string,
	inputTokens int,
	w io.Writer,
	cacheUsage *kiroCacheEmulationUsage,
	enhanced bool,
) error {
	query := kiropkg.ExtractSearchQuery(anthropicBody)
	if strings.TrimSpace(query) == "" {
		return errKiroWebSearchFallback
	}

	currentBody, err := kiropkg.ReplaceWebSearchToolDescription(anthropicBody)
	if err != nil {
		currentBody = anthropicBody
	}
	currentToolUseID := "srvtoolu_" + kiropkg.GenerateToolUseID()
	nextContentBlockIndex := 0

	loopCtx, cancelLoop := kiroWebSearchLoopContext(ctx, enhanced)
	defer cancelLoop()

	if err := writeAnthropicMessageStart(w, "", requestModel, inputTokens, cacheUsage); err != nil {
		return err
	}

	var accUsage kiropkg.Usage
	accStop := ""
	executedSearches := 0

	for iteration := 0; iteration < kiroMaxWebSearchIterations; iteration++ {
		if err := loopCtx.Err(); err != nil {
			return err
		}
		if prefetch != nil {
			prefetch(loopCtx)
		}

		results, mcpErr := client.search(loopCtx, query)
		if mcpErr != nil && !enhanced {
			results = nil
			mcpErr = nil
		}
		if mcpErr != nil {
			if hard := kiroWebSearchHardError(mcpErr); hard != nil {
				return hard
			}
			code := kiroWebSearchToolErrorCode(mcpErr)
			logger.L().Warn("kiro web search mcp failed",
				zap.String("error_code", code),
				zap.Error(mcpErr),
			)
			if err := writeSSEChunks(w, kiropkg.GenerateSearchIndicatorErrorEvents(query, currentToolUseID, code, nextContentBlockIndex)); err != nil {
				return err
			}
			executedSearches++
			return writeKiroWebSearchStreamTermination(w, &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}, executedSearches)
		}

		if err := writeSSEChunks(w, kiropkg.GenerateSearchIndicatorEvents(query, currentToolUseID, results, nextContentBlockIndex)); err != nil {
			return err
		}
		if enhanced {
			executedSearches++
		}
		nextContentBlockIndex += 2

		currentBody, err = kiropkg.InjectToolResultsClaude(currentBody, currentToolUseID, query, results)
		if err != nil {
			return errKiroWebSearchFallback
		}

		round, err := client.complete(loopCtx, currentBody, true, iteration)
		if err != nil {
			if enhanced && iteration > 0 {
				logger.L().Warn("kiro web search upstream failed after first round", zap.Error(err))
				calls := executedSearches
				return writeKiroWebSearchStreamTermination(w, &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}, calls)
			}
			return err
		}
		if round != nil && round.HTTPErr != nil {
			if enhanced && iteration > 0 {
				status := 0
				if round.HTTPErr.Response != nil {
					status = round.HTTPErr.Response.StatusCode
					_ = round.HTTPErr.Response.Body.Close()
				}
				logger.L().Warn("kiro web search upstream status failed after first round", zap.Int("status", status))
				return writeKiroWebSearchStreamTermination(w, &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}, executedSearches)
			}
			return round.HTTPErr
		}
		if round == nil {
			return fmt.Errorf("kiro web search empty round")
		}
		chunks := round.Chunks
		var visible *kiropkg.StreamResult
		if enhanced {
			absorbKiroStreamRound(&accUsage, &accStop, round.Stream)
			visible = &kiropkg.StreamResult{Usage: accUsage, StopReason: accStop}
		} else {
			visible = round.Stream
		}

		analysis := kiropkg.AnalyzeBufferedStream(chunks)
		if analysis.HasWebSearchToolUse && strings.TrimSpace(analysis.WebSearchQuery) != "" && iteration+1 < kiroMaxWebSearchIterations {
			filtered := kiropkg.FilterChunksForClient(chunks, analysis.WebSearchToolUseIndex, nextContentBlockIndex)
			if err := writeSSEChunks(w, filtered); err != nil {
				return err
			}
			if maxIndex := kiropkg.MaxContentBlockIndex(filtered); maxIndex >= nextContentBlockIndex {
				nextContentBlockIndex = maxIndex + 1
			}
			query = analysis.WebSearchQuery
			if strings.TrimSpace(analysis.WebSearchToolUseID) == "" {
				currentToolUseID = "srvtoolu_" + kiropkg.GenerateToolUseID()
			} else {
				currentToolUseID = analysis.WebSearchToolUseID
			}
			continue
		}

		calls := 0
		if enhanced {
			calls = executedSearches
		}
		return writeKiroWebSearchFinalRound(w, chunks, nextContentBlockIndex, visible, calls, enhanced)
	}

	return fmt.Errorf("kiro web search exceeded max iterations")
}

func runKiroOnlyWebSearchNonStream(
	ctx context.Context,
	client kiroRoundClient,
	prefetch func(context.Context),
	anthropicBody []byte,
	requestModel string,
	inputTokens int,
	enhanced bool,
) (*kiroWebSearchExecution, error) {
	query := kiropkg.ExtractSearchQuery(anthropicBody)
	if strings.TrimSpace(query) == "" {
		return nil, errKiroWebSearchFallback
	}

	currentBody, err := kiropkg.ReplaceWebSearchToolDescription(anthropicBody)
	if err != nil {
		currentBody = anthropicBody
	}

	loopCtx, cancelLoop := kiroWebSearchLoopContext(ctx, enhanced)
	defer cancelLoop()

	currentToolUseID := "srvtoolu_" + kiropkg.GenerateToolUseID()
	searches := make([]kiropkg.SearchIndicator, 0, 2)
	requestID := ""
	var accUsage kiropkg.Usage

	for iteration := 0; iteration < kiroMaxWebSearchIterations; iteration++ {
		if err := loopCtx.Err(); err != nil {
			return nil, err
		}
		if prefetch != nil {
			prefetch(loopCtx)
		}

		results, mcpErr := client.search(loopCtx, query)
		if mcpErr != nil && !enhanced {
			results = nil
			mcpErr = nil
		}
		if mcpErr != nil {
			if hard := kiroWebSearchHardError(mcpErr); hard != nil {
				return nil, hard
			}
			code := kiroWebSearchToolErrorCode(mcpErr)
			logger.L().Warn("kiro web search mcp failed",
				zap.String("error_code", code),
				zap.Error(mcpErr),
			)
			searches = append(searches, kiropkg.SearchIndicator{
				ToolUseID: currentToolUseID,
				Query:     query,
				ErrorCode: code,
			})
			usage := kiroUsageToClaude(accUsage, inputTokens)
			return &kiroWebSearchExecution{
				ResponseBody:   kiroWebSearchClientMessage(requestModel, usage, searches, nil),
				Usage:          usage,
				RequestID:      requestID,
				WebSearchCalls: len(searches),
			}, nil
		}
		searches = append(searches, kiropkg.SearchIndicator{
			ToolUseID: currentToolUseID,
			Query:     query,
			Results:   results,
		})

		currentBody, err = kiropkg.InjectToolResultsClaude(currentBody, currentToolUseID, query, results)
		if err != nil {
			return nil, errKiroWebSearchFallback
		}

		round, err := client.complete(loopCtx, currentBody, false, iteration)
		if err != nil {
			if enhanced && iteration > 0 && !kiroWebSearchIsLaterRound(err) {
				return nil, kiroWebSearchLaterRoundError(err)
			}
			return nil, err
		}
		if round != nil && round.HTTPErr != nil {
			return nil, round.HTTPErr
		}
		if round == nil {
			return nil, fmt.Errorf("kiro web search empty round")
		}
		if requestID == "" {
			requestID = round.RequestID
		}
		var usage kiropkg.Usage
		if enhanced {
			mergeKiroRoundUsage(&accUsage, round.Usage)
			usage = accUsage
		} else {
			usage = round.Usage
		}

		nextToolUseID, nextQuery, hasNext := kiropkg.ExtractWebSearchToolUseFromResponse(round.Body)
		if !hasNext || strings.TrimSpace(nextQuery) == "" || iteration+1 >= kiroMaxWebSearchIterations {
			claudeUsage := kiroUsageToClaude(usage, inputTokens)
			body := round.Body
			calls := 0
			if enhanced {
				body = kiroWebSearchClientMessage(requestModel, claudeUsage, searches, round.Body)
				calls = len(searches)
			} else if injected, injectErr := kiropkg.InjectSearchIndicatorsInResponse(round.Body, searches); injectErr == nil {
				body = injected
			}
			return &kiroWebSearchExecution{
				ResponseBody:   body,
				Usage:          claudeUsage,
				RequestID:      requestID,
				WebSearchCalls: calls,
			}, nil
		}

		query = nextQuery
		if strings.TrimSpace(nextToolUseID) == "" {
			nextToolUseID = "srvtoolu_" + kiropkg.GenerateToolUseID()
		}
		currentToolUseID = nextToolUseID
	}

	return nil, fmt.Errorf("kiro web search exceeded max iterations")
}

func (s *GatewayService) prefetchKiroWebSearchDescription(ctx context.Context, account *Account, token string) {
	endpoint := kiropkg.BuildMcpEndpoint(kiroAPIRegion(account))
	if cached, ok := kiroWebSearchDescCache.Load(endpoint); ok {
		if desc, ok := cached.(string); ok && strings.TrimSpace(desc) != "" {
			kiropkg.SetCachedWebSearchDescription(desc)
		}
		return
	}

	reqBody, _ := json.Marshal(kiropkg.MCPRequest{
		ID:      "tools_list",
		JSONRPC: "2.0",
		Method:  "tools/list",
	})
	resp, _, err := s.doKiroMCPJSONRequest(ctx, account, endpoint, reqBody, token)
	if err != nil || resp == nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var result kiropkg.MCPResponse
	if err := json.Unmarshal(body, &result); err != nil || result.Result == nil {
		return
	}
	for _, tool := range result.Result.Tools {
		if strings.EqualFold(tool.Name, "web_search") && strings.TrimSpace(tool.Description) != "" {
			kiroWebSearchDescCache.Store(endpoint, tool.Description)
			kiropkg.SetCachedWebSearchDescription(tool.Description)
			return
		}
	}
}

func (s *GatewayService) callKiroWebSearchMCP(ctx context.Context, account *Account, token, query string) (*kiropkg.WebSearchResults, string, error) {
	reqBody, err := json.Marshal(buildKiroWebSearchMCPRequest(query))
	if err != nil {
		return nil, token, err
	}

	endpoint := kiropkg.BuildMcpEndpoint(kiroAPIRegion(account))
	resp, nextToken, err := s.doKiroMCPJSONRequest(ctx, account, endpoint, reqBody, token)
	if err != nil {
		return nil, nextToken, err
	}
	if resp == nil {
		return nil, nextToken, fmt.Errorf("kiro web search returned nil response")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nextToken, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nextToken, fmt.Errorf("kiro mcp status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed kiropkg.MCPResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nextToken, err
	}
	if parsed.Error != nil {
		msg := "unknown error"
		if parsed.Error.Message != nil && strings.TrimSpace(*parsed.Error.Message) != "" {
			msg = strings.TrimSpace(*parsed.Error.Message)
		}
		code := 0
		if parsed.Error.Code != nil {
			code = *parsed.Error.Code
		}
		return nil, nextToken, fmt.Errorf("kiro mcp error %d: %s", code, msg)
	}

	return kiropkg.ParseSearchResults(&parsed), nextToken, nil
}

func buildKiroWebSearchMCPRequest(query string) kiropkg.MCPRequest {
	return kiropkg.MCPRequest{
		ID:      fmt.Sprintf("web_search_%s", kiropkg.GenerateToolUseID()),
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]any{
			"name": "web_search",
			"arguments": map[string]any{
				"query": query,
				"_meta": map[string]any{
					"_isValid":        true,
					"_activePath":     []string{"query"},
					"_completedPaths": [][]string{{"query"}},
				},
			},
		},
	}
}

func (s *GatewayService) doKiroMCPJSONRequest(ctx context.Context, account *Account, endpoint string, payload []byte, token string) (*http.Response, string, error) {
	currentToken := token
	accountKey := buildKiroAccountKey(account)
	proxyURL := kiroProxyURL(account)
	tlsProfile := s.tlsFPProfileService.ResolveTLSProfile(account)

	for attempt := 0; attempt < 3; attempt++ {
		if err := s.checkKiroCooldown(ctx, accountKey); err != nil {
			if failoverErr := asKiroCooldownFailoverError(err); failoverErr != nil {
				return nil, currentToken, failoverErr
			}
			return nil, currentToken, err
		}

		req, err := newKiroJSONRequest(ctx, endpoint, payload, currentToken, accountKey, buildKiroMachineID(account), "", account)
		if err != nil {
			return nil, currentToken, err
		}

		resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
		if err != nil {
			return nil, currentToken, err
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			respBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, currentToken, readErr
			}
			if resp.StatusCode == http.StatusForbidden && isKiroSuspendedBody(respBody) {
				if _, err := s.markKiroSuspended(ctx, accountKey); err != nil {
					return nil, currentToken, err
				}
				resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
				return resp, currentToken, nil
			}
			if resp.StatusCode == http.StatusForbidden && !isKiroTokenErrorBody(respBody) {
				resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
				return resp, currentToken, nil
			}
			if s.kiroTokenProvider == nil {
				resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
				return resp, currentToken, nil
			}
			refreshedToken, refreshErr := s.kiroTokenProvider.ForceRefreshAccessToken(ctx, account)
			if refreshErr != nil {
				resp.Body = io.NopCloser(strings.NewReader(string(respBody)))
				return resp, currentToken, nil
			}
			currentToken = refreshedToken
			accountKey = buildKiroAccountKey(account)
			if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
				return nil, currentToken, sleepErr
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if _, err := s.markKiro429(ctx, account.ID, accountKey); err != nil {
				_ = resp.Body.Close()
				return nil, currentToken, err
			}
		}
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
			if attempt < 2 {
				_ = resp.Body.Close()
				if sleepErr := sleepKiroRetry(ctx, attempt); sleepErr != nil {
					return nil, currentToken, sleepErr
				}
				continue
			}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := s.markKiroSuccess(ctx, account.ID, accountKey); err != nil {
				_ = resp.Body.Close()
				return nil, currentToken, err
			}
		}

		return resp, currentToken, nil
	}

	return nil, currentToken, fmt.Errorf("kiro mcp request retries exhausted")
}
