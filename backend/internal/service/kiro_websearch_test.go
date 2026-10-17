//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildKiroWebSearchMCPRequest_UsesUnderscoredMetaKeys(t *testing.T) {
	req := buildKiroWebSearchMCPRequest("golang concurrency")

	body, err := json.Marshal(req)
	require.NoError(t, err)

	require.Equal(t, "tools/call", gjson.GetBytes(body, "method").String())
	require.Equal(t, "web_search", gjson.GetBytes(body, "params.name").String())
	require.Equal(t, "golang concurrency", gjson.GetBytes(body, "params.arguments.query").String())
	require.True(t, gjson.GetBytes(body, "params.arguments._meta._isValid").Bool())
	require.Equal(t, "query", gjson.GetBytes(body, "params.arguments._meta._activePath.0").String())
	require.Equal(t, "query", gjson.GetBytes(body, "params.arguments._meta._completedPaths.0.0").String())
	require.False(t, gjson.GetBytes(body, "params.arguments._meta.isValid").Exists())
	require.False(t, gjson.GetBytes(body, "params.arguments._meta.activePath").Exists())
	require.False(t, gjson.GetBytes(body, "params.arguments._meta.completedPaths").Exists())
}

func TestWriteAnthropicMessageStart_UsesCacheEmulationUsage(t *testing.T) {
	var out bytes.Buffer
	err := writeAnthropicMessageStart(&out, "msg_test", "claude-sonnet-4-6", 100, &kiroCacheEmulationUsage{
		InputTokens:              25,
		CacheCreationInputTokens: 75,
		CacheReadInputTokens:     0,
	})
	require.NoError(t, err)
	body := out.String()
	require.Contains(t, body, `"input_tokens":25`)
	require.Contains(t, body, `"cache_creation_input_tokens":75`)
	require.Contains(t, body, `"cache_read_input_tokens":0`)
}

func TestWriteKiroWebSearchFinalRound_EndsWithMessageDeltaAndStop(t *testing.T) {
	chunks := [][]byte{
		[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9999}}}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"found it\"}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
		[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":9999,\"output_tokens\":1}}\n\n"),
		[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
	}

	var out bytes.Buffer
	err := writeKiroWebSearchFinalRound(&out, chunks, 2, &kiropkg.StreamResult{
		StopReason: "end_turn",
		Usage:      kiropkg.Usage{OutputTokens: 15, KiroCredits: 0.17},
	}, 0)
	require.NoError(t, err)

	body := out.String()
	require.NotContains(t, body, `"input_tokens":9999`)
	require.Contains(t, body, `"text":"found it"`)
	require.Contains(t, body, `"index":2`)
	deltaAt := strings.Index(body, "event: message_delta")
	stopAt := strings.Index(body, "event: message_stop")
	require.GreaterOrEqual(t, deltaAt, 0)
	require.Greater(t, stopAt, deltaAt)
	require.Contains(t, body, `"stop_reason":"end_turn"`)
	require.Contains(t, body, `"output_tokens":15`)
	require.Contains(t, body, `"_sub2api_kiro_credits":0.17`)
	require.Equal(t, stopAt, strings.LastIndex(body, "event: message_stop"))
}

func TestHandleStreamingResponse_KiroWebSearchFinalRoundRecordsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newMinimalGatewayService()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}
	go func() {
		defer func() { _ = pw.Close() }()
		if err := writeAnthropicMessageStart(pw, "msg_ws", "claude-sonnet-4-6", 100, &kiroCacheEmulationUsage{
			InputTokens:              25,
			CacheCreationInputTokens: 75,
		}); err != nil {
			return
		}
		_ = writeKiroWebSearchFinalRound(pw, [][]byte{
			[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":9999,\"output_tokens\":1}}\n\n"),
			[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n"),
		}, 0, &kiropkg.StreamResult{
			StopReason: "end_turn",
			Usage:      kiropkg.Usage{OutputTokens: 15, KiroCredits: 0.17},
		}, 1)
	}()

	result, err := svc.handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-sonnet-4-6", "claude-sonnet-4-6", false)
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 25, result.usage.InputTokens)
	require.Equal(t, 75, result.usage.CacheCreationInputTokens)
	require.Equal(t, 15, result.usage.OutputTokens)
	require.InDelta(t, 0.17, result.usage.KiroCredits, 0.000001)
	require.Equal(t, 1, result.usage.WebSearchRequests)

	body := rec.Body.String()
	require.Contains(t, body, "event: message_delta")
	require.Contains(t, body, "event: message_stop")
	require.Contains(t, body, `"output_tokens":15`)
	require.Contains(t, body, `"web_search_requests":1`)
	require.Contains(t, body, `"stop_reason":"end_turn"`)
	require.NotContains(t, body, `"input_tokens":9999`)
	require.NotContains(t, body, "_sub2api_kiro_credits")
	require.Greater(t, strings.Index(body, "event: message_stop"), strings.Index(body, "event: message_delta"))
}
