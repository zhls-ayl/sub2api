//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHasMixedKiroWebSearchTools_ClaudeCodeStyleFixture(t *testing.T) {
	body := claudeCodeStyleWebSearchBody()
	require.True(t, hasMixedKiroWebSearchTools(body))
	require.False(t, isOnlyWebSearchToolInBody(body))
	require.True(t, isOnlyWebSearchToolInBody(onlyWebSearchBody()))
	require.False(t, hasMixedKiroWebSearchTools(onlyWebSearchBody()))
}

func TestKiroMixedToolWebSearchActive_DefaultOffAndGroupOverride(t *testing.T) {
	kiroGroup := &Group{ID: 7, Platform: PlatformKiro}
	anthropicGroup := &Group{ID: 7, Platform: PlatformAnthropic}
	require.False(t, kiroMixedToolWebSearchActive(nil, kiroGroup))
	require.False(t, kiroMixedToolWebSearchActive(&KiroMixedToolWebSearchConfig{Enabled: false}, kiroGroup))
	require.True(t, kiroMixedToolWebSearchActive(&KiroMixedToolWebSearchConfig{Enabled: true}, kiroGroup))
	require.False(t, kiroMixedToolWebSearchActive(&KiroMixedToolWebSearchConfig{Enabled: true}, anthropicGroup))
	require.True(t, kiroMixedToolWebSearchActive(&KiroMixedToolWebSearchConfig{
		Enabled:    false,
		GroupModes: map[string]string{"7": "enabled"},
	}, kiroGroup))
	require.False(t, kiroMixedToolWebSearchActive(&KiroMixedToolWebSearchConfig{
		Enabled:    true,
		GroupModes: map[string]string{"7": "disabled"},
	}, kiroGroup))
	require.True(t, kiroMixedToolWebSearchActive(&KiroMixedToolWebSearchConfig{Enabled: true}, &Group{ID: 8, Platform: ""}))
}

func TestSaveKiroMixedToolWebSearchConfig_PreservesGroupModesUntilCleared(t *testing.T) {
	resetKiroMixedToolWebSearchCache()
	repo := &mapSettingRepo{values: map[string]string{}}
	svc := &SettingService{settingRepo: repo}

	saved, err := svc.SaveKiroMixedToolWebSearchConfig(context.Background(), true, nil)
	require.NoError(t, err)
	require.True(t, saved.Enabled)
	require.Empty(t, saved.GroupModes)

	saved, err = svc.SetKiroMixedToolWebSearchGroupMode(context.Background(), 7, "enabled")
	require.NoError(t, err)
	require.Equal(t, "enabled", saved.GroupModes["7"])

	saved, err = svc.SaveKiroMixedToolWebSearchConfig(context.Background(), false, nil)
	require.NoError(t, err)
	require.False(t, saved.Enabled)
	require.Equal(t, "enabled", saved.GroupModes["7"])

	empty := map[string]string{}
	saved, err = svc.SaveKiroMixedToolWebSearchConfig(context.Background(), false, &empty)
	require.NoError(t, err)
	require.Empty(t, saved.GroupModes)

	_, err = svc.SetKiroMixedToolWebSearchGroupMode(context.Background(), 7, "sometimes")
	require.Error(t, err)
}

func TestRunKiroMixedNonStream_OnlyWebSearchThenText(t *testing.T) {
	client := &scriptedKiroRoundClient{
		rounds: []scriptedRound{
			{
				body:  claudeToolMessage("tool_use", remoteWebSearchTool("toolu_search", "golang release")),
				usage: kiropkg.Usage{InputTokens: 20, OutputTokens: 4, KiroCredits: 0.1},
			},
			{
				body:  claudeTextMessage("end_turn", "the current release is Go 1.23"),
				usage: kiropkg.Usage{InputTokens: 30, OutputTokens: 6, KiroCredits: 0.2},
			},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{Results: []kiropkg.WebSearchResult{{Title: "Go", URL: "https://go.dev"}}}}},
	}
	result, err := runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 100)
	require.NoError(t, err)
	require.Equal(t, 1, result.WebSearchCalls)
	require.Equal(t, 30, result.Usage.InputTokens)
	require.Equal(t, 10, result.Usage.OutputTokens)
	require.InDelta(t, 0.3, result.Usage.KiroCredits, 0.000001)
	body := string(result.ResponseBody)
	require.Contains(t, body, `"type":"server_tool_use"`)
	require.Contains(t, body, `"type":"web_search_tool_result"`)
	require.Contains(t, body, "the current release is Go 1.23")
	require.Contains(t, body, `"web_search_requests":1`)
	require.NotContains(t, body, "remote_web_search")
	require.Len(t, client.queries, 1)
	require.Equal(t, "golang release", client.queries[0])
	require.Contains(t, string(client.bodies[1]), `"type":"tool_result"`)
	require.Contains(t, string(client.bodies[1]), "web_search_20250305")
}

func TestRunKiroMixedNonStream_MixedToolTurnStops(t *testing.T) {
	client := &scriptedKiroRoundClient{
		rounds: []scriptedRound{{
			body: claudeToolMessage("tool_use",
				remoteWebSearchTool("toolu_search", "golang release"),
				bashTool("toolu_bash", "ls"),
			),
			usage: kiropkg.Usage{InputTokens: 11, OutputTokens: 5},
		}},
	}
	result, err := runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 40)
	require.NoError(t, err)
	require.Equal(t, 0, result.WebSearchCalls)
	require.Empty(t, client.queries)
	body := string(result.ResponseBody)
	require.Contains(t, body, `"name":"web_search"`)
	require.Contains(t, body, `"name":"Bash"`)
	require.NotContains(t, body, "remote_web_search")
	require.NotContains(t, body, "server_tool_use")
	require.Contains(t, body, `"stop_reason":"tool_use"`)
}

func TestRunKiroMixedNonStream_IterationCapHandsOffFifthSearch(t *testing.T) {
	rounds := make([]scriptedRound, 0, kiroMaxWebSearchIterations)
	searches := make([]scriptedSearch, 0, kiroMaxWebSearchIterations-1)
	for i := 0; i < kiroMaxWebSearchIterations; i++ {
		rounds = append(rounds, scriptedRound{
			body:  claudeToolMessage("tool_use", remoteWebSearchTool(fmt.Sprintf("toolu_%d", i), fmt.Sprintf("query-%d", i))),
			usage: kiropkg.Usage{OutputTokens: 1, KiroCredits: 0.01},
		})
		if i+1 < kiroMaxWebSearchIterations {
			searches = append(searches, scriptedSearch{results: &kiropkg.WebSearchResults{}})
		}
	}
	client := &scriptedKiroRoundClient{rounds: rounds, searches: searches}
	result, err := runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 10)
	require.NoError(t, err)
	require.Equal(t, kiroMaxWebSearchIterations-1, result.WebSearchCalls)
	require.Len(t, client.queries, kiroMaxWebSearchIterations-1)
	require.Equal(t, kiroMaxWebSearchIterations, result.Usage.OutputTokens)
	body := string(result.ResponseBody)
	require.Contains(t, body, `"type":"tool_use"`)
	require.Contains(t, body, "query-4")
	require.NotContains(t, body, "remote_web_search")
	require.Equal(t, kiroMaxWebSearchIterations-1, strings.Count(body, `"type":"server_tool_use"`))
}

func TestRunKiroMixedNonStream_MCPFailureSurfacesErrorAndStops(t *testing.T) {
	client := &scriptedKiroRoundClient{
		rounds: []scriptedRound{{
			body:  claudeTextAndToolMessage("searching the docs", remoteWebSearchTool("toolu_search", "golang release")),
			usage: kiropkg.Usage{InputTokens: 8, OutputTokens: 2},
		}},
		searches: []scriptedSearch{{err: fmt.Errorf("kiro mcp status 500: boom")}},
	}
	result, err := runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 12)
	require.NoError(t, err)
	require.Equal(t, 1, result.WebSearchCalls)
	require.Len(t, client.rounds, 1)
	require.Equal(t, 1, client.ri)
	body := string(result.ResponseBody)
	require.Contains(t, body, `"error_code":"unavailable"`)
	require.Contains(t, body, `"type":"web_search_tool_result_error"`)
	require.Contains(t, body, "searching the docs")
	require.NotContains(t, body, `"type":"tool_use"`)

	client = &scriptedKiroRoundClient{
		rounds: []scriptedRound{{
			body:  claudeToolMessage("tool_use", remoteWebSearchTool("toolu_search", "golang release")),
			usage: kiropkg.Usage{OutputTokens: 2},
		}},
		searches: []scriptedSearch{{err: fmt.Errorf("kiro mcp status 429: slow down")}},
	}
	result, err = runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 12)
	require.NoError(t, err)
	require.Contains(t, string(result.ResponseBody), `"error_code":"too_many_requests"`)

	client = &scriptedKiroRoundClient{
		rounds: []scriptedRound{{
			body: claudeToolMessage("tool_use", remoteWebSearchTool("toolu_search", "golang release")),
		}},
		searches: []scriptedSearch{{err: context.DeadlineExceeded}},
	}
	_, err = runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 12)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunKiroMixedNonStream_FirstHTTPErrorFailoversWithoutCommit(t *testing.T) {
	client := &scriptedKiroRoundClient{rounds: []scriptedRound{{httpStatus: 503}}}
	_, err := runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 12)
	var httpErr *kiroWebSearchHTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, 503, httpErr.Response.StatusCode)

	client = &scriptedKiroRoundClient{rounds: []scriptedRound{
		{body: claudeToolMessage("tool_use", remoteWebSearchTool("toolu_search", "golang")), usage: kiropkg.Usage{OutputTokens: 1}},
		{httpStatus: 502},
	}, searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{}}}}
	_, err = runKiroMixedNonStream(context.Background(), client, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 12)
	require.Error(t, err)
	var laterHTTP *kiroWebSearchHTTPError
	require.ErrorAs(t, err, &laterHTTP)
	require.Equal(t, 502, laterHTTP.Response.StatusCode)
	require.Equal(t, "later_round", kiroWebSearchNonStreamFailureAction(err))
	require.Equal(t, "failover_http", kiroWebSearchNonStreamFailureAction(&kiroWebSearchHTTPError{Response: &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("x"))}}))
}

func TestRunKiroMixedStream_EndsWithMessageDeltaAndRecordsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &scriptedKiroRoundClient{
		rounds: []scriptedRound{
			{
				chunks: webSearchToolChunks(0, "toolu_search", "remote_web_search", "golang release"),
				stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 4, KiroCredits: 0.1}},
			},
			{
				chunks: textChunks(0, "found it"),
				stream: &kiropkg.StreamResult{StopReason: "end_turn", Usage: kiropkg.Usage{OutputTokens: 6, KiroCredits: 0.07}},
			},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{Results: []kiropkg.WebSearchResult{{Title: "Go", URL: "https://go.dev"}}}}},
	}
	var buf bytes.Buffer
	err := runKiroMixedStream(context.Background(), client, &buf, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 100, &kiroCacheEmulationUsage{
		InputTokens:              25,
		CacheCreationInputTokens: 75,
	})
	require.NoError(t, err)
	raw := buf.String()
	require.Contains(t, raw, "event: message_delta")
	require.Contains(t, raw, "event: message_stop")
	require.Greater(t, strings.Index(raw, "event: message_stop"), strings.LastIndex(raw, "event: message_delta"))
	require.Contains(t, raw, `"output_tokens":10`)
	require.Contains(t, raw, `"stop_reason":"end_turn"`)
	require.Contains(t, raw, `"web_search_requests":1`)
	require.Contains(t, raw, `"type":"server_tool_use"`)
	require.Contains(t, raw, "found it")
	require.NotContains(t, raw, "remote_web_search")
	require.Contains(t, raw, `"index":2`)

	svc := newMinimalGatewayService()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(buf.Bytes()))}
	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-sonnet-4-6", "claude-sonnet-4-6", false)
	require.NoError(t, err)
	require.Equal(t, 25, result.usage.InputTokens)
	require.Equal(t, 75, result.usage.CacheCreationInputTokens)
	require.Equal(t, 10, result.usage.OutputTokens)
	require.InDelta(t, 0.17, result.usage.KiroCredits, 0.000001)
	require.Equal(t, 1, result.usage.WebSearchRequests)
	require.NotContains(t, rec.Body.String(), "_sub2api_kiro_credits")
}

func TestRunKiroMixedStream_MixedToolTurnStopsWithoutSearch(t *testing.T) {
	chunks := append(webSearchToolChunks(0, "toolu_search", "remote_web_search", "golang release"), bashToolChunks(1, "toolu_bash")...)
	client := &scriptedKiroRoundClient{rounds: []scriptedRound{{
		chunks: chunks,
		stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 3}},
	}}}
	var buf bytes.Buffer
	err := runKiroMixedStream(context.Background(), client, &buf, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 10, nil)
	require.NoError(t, err)
	require.Empty(t, client.queries)
	body := buf.String()
	require.Contains(t, body, `"name":"web_search"`)
	require.Contains(t, body, `"name":"Bash"`)
	require.NotContains(t, body, "remote_web_search")
	require.Contains(t, body, `"stop_reason":"tool_use"`)
	require.Contains(t, body, "event: message_stop")
	require.NotContains(t, body, "web_search_requests")
}

func TestRunKiroMixedStream_FirstHTTPErrorWritesNothing(t *testing.T) {
	client := &scriptedKiroRoundClient{rounds: []scriptedRound{{httpStatus: 500}}}
	var buf bytes.Buffer
	err := runKiroMixedStream(context.Background(), client, &buf, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 10, nil)
	require.ErrorAs(t, err, new(*kiroWebSearchHTTPError))
	require.Empty(t, buf.String())
}

func TestGatewayKiroRoundClient_DoesNotCommitCacheOnFirstNon2xx(t *testing.T) {
	commits := 0
	client := &gatewayKiroRoundClient{
		upstream: func(context.Context, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("upstream down"))}, nil
		},
		commitCache: func() { commits++ },
	}
	round, err := client.complete(context.Background(), []byte(`{"messages":[]}`), true, 0)
	require.NoError(t, err)
	require.NotNil(t, round.HTTPErr)
	require.Equal(t, 0, commits)

	client.upstream = func(context.Context, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	_, err = client.complete(context.Background(), []byte(`{"messages":[]}`), true, 0)
	require.NoError(t, err)
	require.Equal(t, 1, commits)

	_, err = client.complete(context.Background(), []byte(`{"messages":[]}`), true, 1)
	require.NoError(t, err)
	require.Equal(t, 1, commits)
}

func TestApplyWebSearchCallSurcharge_NilPriceSkipsDefault(t *testing.T) {
	billing := &BillingService{}
	base := &CostBreakdown{TotalCost: 1, ActualCost: 1}
	got := applyWebSearchCallSurcharge(base, billing, 2, nil, 1)
	require.Equal(t, 1.0, got.ActualCost)
	require.Equal(t, 1.0, got.TotalCost)

	price := 0.02
	zero := &CostBreakdown{ActualCost: 0}
	got = applyWebSearchCallSurcharge(zero, billing, 3, &price, 1)
	require.InDelta(t, 0.06, got.ActualCost, 1e-9)
	require.InDelta(t, 0.06, got.TotalCost, 1e-9)

	free := 0.0
	priced := &CostBreakdown{TotalCost: 2, ActualCost: 2}
	got = applyWebSearchCallSurcharge(priced, billing, 4, &free, 1)
	require.Equal(t, 2.0, got.ActualCost)

	negative := -1.0
	negativeBase := &CostBreakdown{TotalCost: 2, ActualCost: 2}
	got = applyWebSearchCallSurcharge(negativeBase, billing, 3, &negative, 1)
	require.Equal(t, 2.0, got.ActualCost)
	require.Equal(t, 2.0, got.TotalCost)
}

func TestOnlyWebSearchSwitchOffMatchesMainBehavior(t *testing.T) {
	plain, cancel := kiroWebSearchLoopContext(context.Background(), false)
	defer cancel()
	_, hasDeadline := plain.Deadline()
	require.False(t, hasDeadline)
	limited, cancelLimited := kiroWebSearchLoopContext(context.Background(), true)
	defer cancelLimited()
	_, hasDeadline = limited.Deadline()
	require.True(t, hasDeadline)

	offMCP := &scriptedKiroRoundClient{
		rounds: []scriptedRound{{
			body:  claudeTextMessage("end_turn", "still answered"),
			usage: kiropkg.Usage{InputTokens: 9, OutputTokens: 3, KiroCredits: 0.2},
		}},
		searches: []scriptedSearch{{err: fmt.Errorf("kiro mcp status 500: boom")}},
	}
	offResult, err := runKiroOnlyWebSearchNonStream(context.Background(), offMCP, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, false)
	require.NoError(t, err)
	require.False(t, offMCP.sawSearchDeadline)
	require.Equal(t, 1, offMCP.ri)
	require.Equal(t, 0, offResult.WebSearchCalls)
	require.Equal(t, 3, offResult.Usage.OutputTokens)
	require.NotContains(t, string(offResult.ResponseBody), "web_search_tool_result_error")
	require.NotContains(t, string(offResult.ResponseBody), "web_search_requests")
	require.Contains(t, string(offResult.ResponseBody), "still answered")

	onMCP := &scriptedKiroRoundClient{
		searches: []scriptedSearch{{err: fmt.Errorf("kiro mcp status 500: boom")}},
	}
	onResult, err := runKiroOnlyWebSearchNonStream(context.Background(), onMCP, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, true)
	require.NoError(t, err)
	require.True(t, onMCP.sawSearchDeadline)
	require.Equal(t, 0, onMCP.ri)
	require.Equal(t, 1, onResult.WebSearchCalls)
	require.Contains(t, string(onResult.ResponseBody), `"error_code":"unavailable"`)

	finalWithAlias := claudeTextAndToolMessage("answer", remoteWebSearchTool("toolu_final", ""))
	rounds := []scriptedRound{
		{body: claudeToolMessage("tool_use", remoteWebSearchTool("toolu_1", "next")), usage: kiropkg.Usage{InputTokens: 20, OutputTokens: 4, KiroCredits: 0.25}},
		{body: finalWithAlias, usage: kiropkg.Usage{InputTokens: 0, OutputTokens: 6, KiroCredits: 0.25}},
	}
	searches := []scriptedSearch{
		{results: &kiropkg.WebSearchResults{}},
		{results: &kiropkg.WebSearchResults{}},
	}
	offUsage := &scriptedKiroRoundClient{preserveLaterRoundErrors: true, rounds: rounds, searches: searches}
	offGot, err := runKiroOnlyWebSearchNonStream(context.Background(), offUsage, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, false)
	require.NoError(t, err)
	require.Equal(t, 0, offGot.WebSearchCalls)
	require.Equal(t, 6, offGot.Usage.OutputTokens)
	require.InDelta(t, 0.25, offGot.Usage.KiroCredits, 1e-9)
	require.Equal(t, 100, offGot.Usage.InputTokens)
	require.Contains(t, string(offGot.ResponseBody), "remote_web_search")
	require.NotContains(t, string(offGot.ResponseBody), "web_search_requests")

	onUsage := &scriptedKiroRoundClient{rounds: append([]scriptedRound(nil), rounds...), searches: append([]scriptedSearch(nil), searches...)}
	onGot, err := runKiroOnlyWebSearchNonStream(context.Background(), onUsage, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, true)
	require.NoError(t, err)
	require.Equal(t, 2, onGot.WebSearchCalls)
	require.Equal(t, 10, onGot.Usage.OutputTokens)
	require.InDelta(t, 0.5, onGot.Usage.KiroCredits, 1e-9)
	require.Equal(t, 20, onGot.Usage.InputTokens)
	require.NotContains(t, string(onGot.ResponseBody), "remote_web_search")
	require.Contains(t, string(onGot.ResponseBody), `"web_search_requests":2`)
}

func TestOnlyWebSearchStreamSwitchOffKeepsLegacyTermination(t *testing.T) {
	offFail := &scriptedKiroRoundClient{
		preserveLaterRoundErrors: true,
		rounds: []scriptedRound{
			{chunks: webSearchToolChunks(0, "toolu_1", "remote_web_search", "next"), stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 4}}},
			{err: errors.New("upstream reset")},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{}}},
	}
	var buf bytes.Buffer
	err := runKiroOnlyWebSearchStream(context.Background(), offFail, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, &buf, nil, false)
	require.EqualError(t, err, "upstream reset")
	require.False(t, kiroWebSearchIsLaterRound(err))
	require.False(t, offFail.sawSearchDeadline)
	require.NotContains(t, buf.String(), "event: error")
	require.NotContains(t, buf.String(), "event: message_stop")

	onFail := &scriptedKiroRoundClient{
		rounds: []scriptedRound{
			{chunks: webSearchToolChunks(0, "toolu_1", "remote_web_search", "next"), stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 4, KiroCredits: 0.1}}},
			{err: errors.New("upstream reset")},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{}}},
	}
	buf.Reset()
	err = runKiroOnlyWebSearchStream(context.Background(), onFail, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, &buf, nil, true)
	require.NoError(t, err)
	require.True(t, onFail.sawSearchDeadline)
	require.NotContains(t, buf.String(), "event: error")
	require.Contains(t, buf.String(), "event: message_stop")
	require.Contains(t, buf.String(), `"output_tokens":4`)

	offMCP := &scriptedKiroRoundClient{
		rounds: []scriptedRound{{
			chunks: textChunks(0, "after empty search"),
			stream: &kiropkg.StreamResult{StopReason: "end_turn", Usage: kiropkg.Usage{OutputTokens: 2}},
		}},
		searches: []scriptedSearch{{err: fmt.Errorf("kiro mcp status 500: boom")}},
	}
	buf.Reset()
	err = runKiroOnlyWebSearchStream(context.Background(), offMCP, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, &buf, nil, false)
	require.NoError(t, err)
	require.Equal(t, 1, offMCP.ri)
	require.NotContains(t, buf.String(), "web_search_tool_result_error")
	require.NotContains(t, buf.String(), "web_search_requests")
	require.Contains(t, buf.String(), "after empty search")
	require.Contains(t, buf.String(), "event: message_stop")

	aliasChunks := webSearchToolChunks(0, "toolu_final", "remote_web_search", "")
	offOK := &scriptedKiroRoundClient{
		rounds: []scriptedRound{
			{chunks: webSearchToolChunks(0, "toolu_1", "web_search", "next"), stream: &kiropkg.StreamResult{Usage: kiropkg.Usage{OutputTokens: 4, KiroCredits: 0.25}}},
			{chunks: aliasChunks, stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 6, KiroCredits: 0.25}}},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{}}, {results: &kiropkg.WebSearchResults{}}},
	}
	buf.Reset()
	err = runKiroOnlyWebSearchStream(context.Background(), offOK, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, &buf, nil, false)
	require.NoError(t, err)
	require.Contains(t, buf.String(), `"output_tokens":6`)
	require.NotContains(t, buf.String(), "web_search_requests")
	require.Contains(t, buf.String(), "remote_web_search")
	require.Contains(t, buf.String(), "event: message_stop")

	onOK := &scriptedKiroRoundClient{
		rounds: []scriptedRound{
			{chunks: webSearchToolChunks(0, "toolu_1", "web_search", "next"), stream: &kiropkg.StreamResult{Usage: kiropkg.Usage{OutputTokens: 4, KiroCredits: 0.25}}},
			{chunks: aliasChunks, stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 6, KiroCredits: 0.25}}},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{}}, {results: &kiropkg.WebSearchResults{}}},
	}
	buf.Reset()
	err = runKiroOnlyWebSearchStream(context.Background(), onOK, nil, onlyWebSearchRequestBody("golang"), "claude-sonnet-4-6", 100, &buf, nil, true)
	require.NoError(t, err)
	require.Contains(t, buf.String(), `"output_tokens":10`)
	require.Contains(t, buf.String(), `"web_search_requests":2`)
	require.NotContains(t, buf.String(), "remote_web_search")
	require.NotContains(t, buf.String(), "event: error")
}

func TestForward_KiroLaterRoundFailureBillsCompletedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &scriptedKiroRoundClient{
		rounds: []scriptedRound{
			{
				chunks: webSearchToolChunks(0, "toolu_search", "remote_web_search", "golang release"),
				stream: &kiropkg.StreamResult{StopReason: "tool_use", Usage: kiropkg.Usage{OutputTokens: 4, KiroCredits: 0.1}},
			},
			{err: errors.New("upstream reset")},
		},
		searches: []scriptedSearch{{results: &kiropkg.WebSearchResults{Results: []kiropkg.WebSearchResult{{Title: "Go", URL: "https://go.dev"}}}}},
	}
	var buf bytes.Buffer
	err := runKiroMixedStream(context.Background(), client, &buf, claudeCodeStyleWebSearchBody(), "claude-sonnet-4-6", 100, &kiroCacheEmulationUsage{
		InputTokens:              25,
		CacheCreationInputTokens: 75,
	})
	require.NoError(t, err)
	raw := buf.String()
	require.NotContains(t, raw, "event: error")
	require.Contains(t, raw, "event: message_stop")
	require.Contains(t, raw, `"output_tokens":4`)

	forward := func(sse string) (*ForwardResult, error, string) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		body := []byte(`{"model":"claude-3-5-sonnet-latest","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
		parsed, parseErr := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
		require.NoError(t, parseErr)
		upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"X-Request-Id": []string{"rid-kiro-later"},
			},
			Body: io.NopCloser(strings.NewReader(sse)),
		}}
		svc := newForwardPartialUsageServiceForTest(upstream)
		result, fwdErr := svc.Forward(context.Background(), c, newAnthropicOAuthAccountForPartialUsageTest(), parsed)
		return result, fwdErr, rec.Body.String()
	}

	result, fwdErr, clientBody := forward(raw)
	require.NoError(t, fwdErr)
	require.NotNil(t, result)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, 25, result.Usage.InputTokens)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(fwdErr, &failover))
	require.Contains(t, clientBody, "event: message_stop")
	require.NotContains(t, clientBody, "event: error")

	poisoned := strings.Replace(raw, "event: message_delta", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Upstream request failed\"}}\n\nevent: message_delta", 1)
	result, fwdErr, _ = forward(poisoned)
	require.Error(t, fwdErr)
	require.ErrorAs(t, fwdErr, &failover)
	require.Nil(t, result)
}

func TestGatewayKiroRoundClient_LaterRoundWrapsCause(t *testing.T) {
	client := &gatewayKiroRoundClient{
		enhanced: true,
		upstream: func(context.Context, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("down"))}, nil
		},
	}
	_, err := client.complete(context.Background(), []byte(`{"messages":[]}`), true, 1)
	require.Error(t, err)
	require.True(t, kiroWebSearchIsLaterRound(err))
	var httpErr *kiroWebSearchHTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusBadGateway, httpErr.Response.StatusCode)
	require.Equal(t, "later_round", kiroWebSearchNonStreamFailureAction(err))
	require.EqualError(t, errors.Unwrap(err), httpErr.Error())

	client.upstream = func(context.Context, []byte) (*http.Response, error) {
		return nil, errors.New("dial failed")
	}
	_, err = client.complete(context.Background(), []byte(`{"messages":[]}`), true, 1)
	require.True(t, kiroWebSearchIsLaterRound(err))
	require.EqualError(t, errors.Unwrap(err), "dial failed")

	wrappedFailover := kiroWebSearchLaterRoundError(&UpstreamFailoverError{StatusCode: http.StatusPaymentRequired})
	require.Equal(t, "later_round", kiroWebSearchNonStreamFailureAction(wrappedFailover))
	require.ErrorAs(t, wrappedFailover, new(*UpstreamFailoverError))

	client.enhanced = false
	client.upstream = func(context.Context, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("down"))}, nil
	}
	round, err := client.complete(context.Background(), []byte(`{"messages":[]}`), true, 1)
	require.NoError(t, err)
	require.NotNil(t, round.HTTPErr)
	require.Equal(t, "failover_http", kiroWebSearchNonStreamFailureAction(round.HTTPErr))
}

type scriptedRound struct {
	err        error
	httpStatus int
	chunks     [][]byte
	stream     *kiropkg.StreamResult
	body       []byte
	usage      kiropkg.Usage
}

type scriptedSearch struct {
	results *kiropkg.WebSearchResults
	err     error
}

type scriptedKiroRoundClient struct {
	rounds                   []scriptedRound
	searches                 []scriptedSearch
	bodies                   [][]byte
	queries                  []string
	ri                       int
	si                       int
	preserveLaterRoundErrors bool
	sawSearchDeadline        bool
}

func (c *scriptedKiroRoundClient) complete(ctx context.Context, body []byte, _ bool, iteration int) (*kiroModelRound, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.bodies = append(c.bodies, append([]byte(nil), body...))
	if c.ri >= len(c.rounds) {
		return nil, fmt.Errorf("unexpected model round %d", c.ri)
	}
	round := c.rounds[c.ri]
	c.ri++
	if round.err != nil {
		if iteration > 0 && !c.preserveLaterRoundErrors {
			return nil, kiroWebSearchLaterRoundError(round.err)
		}
		return nil, round.err
	}
	if round.httpStatus >= 400 {
		resp := &http.Response{StatusCode: round.httpStatus, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("fail"))}
		if iteration > 0 && !c.preserveLaterRoundErrors {
			_ = resp.Body.Close()
			return nil, kiroWebSearchLaterRoundError(&kiroWebSearchHTTPError{Response: resp})
		}
		return &kiroModelRound{HTTPErr: &kiroWebSearchHTTPError{Response: resp}}, nil
	}
	return &kiroModelRound{
		Chunks:     round.chunks,
		Stream:     round.stream,
		Body:       round.body,
		Usage:      round.usage,
		StopReason: round.streamStop(),
		RequestID:  "req_test",
	}, nil
}

func (r scriptedRound) streamStop() string {
	if r.stream != nil && strings.TrimSpace(r.stream.StopReason) != "" {
		return r.stream.StopReason
	}
	return ""
}

func (c *scriptedKiroRoundClient) search(ctx context.Context, query string) (*kiropkg.WebSearchResults, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); ok {
		c.sawSearchDeadline = true
	}
	c.queries = append(c.queries, query)
	if c.si >= len(c.searches) {
		return nil, fmt.Errorf("unexpected search %d", c.si)
	}
	item := c.searches[c.si]
	c.si++
	return item.results, item.err
}

func claudeCodeStyleWebSearchBody() []byte {
	tools := []any{
		map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
	}
	for i := 0; i < 8; i++ {
		tools = append(tools, map[string]any{
			"name":         fmt.Sprintf("Tool%d", i),
			"description":  "fixture tool",
			"input_schema": map[string]any{"type": "object"},
		})
	}
	tools = append(tools, map[string]any{
		"name":         "Bash",
		"description":  "run a command",
		"input_schema": map[string]any{"type": "object"},
	})
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "find the go release notes and list the repo"},
		},
		"tools": tools,
	})
	if err != nil {
		panic(err)
	}
	return body
}

func onlyWebSearchRequestBody(query string) []byte {
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "Perform a web search for the query: " + query},
		},
		"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func onlyWebSearchBody() []byte {
	body, err := json.Marshal(map[string]any{
		"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func remoteWebSearchTool(id, query string) map[string]any {
	return map[string]any{
		"type":  "tool_use",
		"id":    id,
		"name":  "remote_web_search",
		"input": map[string]any{"query": query},
	}
}

func bashTool(id, command string) map[string]any {
	return map[string]any{
		"type":  "tool_use",
		"id":    id,
		"name":  "Bash",
		"input": map[string]any{"command": command},
	}
}

func claudeToolMessage(stop string, tools ...map[string]any) []byte {
	content := make([]any, 0, len(tools))
	for _, tool := range tools {
		content = append(content, tool)
	}
	body, err := json.Marshal(map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-sonnet-4-6",
		"stop_reason": stop,
		"content":     content,
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func claudeTextAndToolMessage(text string, tool map[string]any) []byte {
	body, err := json.Marshal(map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-sonnet-4-6",
		"stop_reason": "tool_use",
		"content": []any{
			map[string]any{"type": "text", "text": text},
			tool,
		},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func claudeTextMessage(stop, text string) []byte {
	body, err := json.Marshal(map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-sonnet-4-6",
		"stop_reason": stop,
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func sseEvent(eventType string, payload any) []byte {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return []byte("event: " + eventType + "\ndata: " + string(raw) + "\n\n")
}

func webSearchToolChunks(index int, id, name, query string) [][]byte {
	return [][]byte{
		sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"query":"` + query + `"}`},
		}),
		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}),
	}
}

func bashToolChunks(index int, id string) [][]byte {
	return [][]byte{
		sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{}},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"command":"ls"}`},
		}),
		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}),
	}
}

func textChunks(index int, text string) [][]byte {
	return [][]byte{
		sseEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}),
		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}),
	}
}

func resetKiroMixedToolWebSearchCache() {
	kiroMixedToolWebSearchCache.Store(&cachedKiroMixedToolWebSearchConfig{expiresAt: 0})
}

type mapSettingRepo struct {
	values map[string]string
}

func (r *mapSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (r *mapSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *mapSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}
func (r *mapSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *mapSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *mapSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *mapSettingRepo) Delete(context.Context, string) error { return nil }
