//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardKiroMessagesPromptTooLong(t *testing.T) {
	shortBody := kiroPromptAnthropicBody([]any{
		map[string]any{"role": "user", "content": "hello"},
	})
	oversizedBody := kiroPromptAnthropicBody([]any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_large",
				"name":  "write_file",
				"input": map[string]any{"content": strings.Repeat("X", 900*1024+64*1024)},
			},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_large", "content": "done"},
			map[string]any{"type": "text", "text": "continue"},
		}},
	})
	contentLengthBody := `{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD","message":"Input is too long."}`

	tests := []struct {
		name         string
		stream       bool
		maxInput     int64
		limitModel   string
		body         []byte
		upstreamBody string
		wantUpstream int
		wantNumbers  bool
	}{
		{
			name:         "over max input tokens",
			maxInput:     1,
			limitModel:   "claude-sonnet-4-5",
			body:         shortBody,
			wantUpstream: 0,
			wantNumbers:  true,
		},
		{
			name:         "over max input tokens stream",
			stream:       true,
			maxInput:     1,
			limitModel:   "claude-sonnet-4-5",
			body:         shortBody,
			wantUpstream: 0,
			wantNumbers:  true,
		},
		{
			name:         "byte truncation cannot fit",
			body:         oversizedBody,
			wantUpstream: 0,
		},
		{
			name:         "byte truncation cannot fit stream",
			stream:       true,
			body:         oversizedBody,
			wantUpstream: 0,
		},
		{
			name:         "upstream content length",
			body:         shortBody,
			upstreamBody: contentLengthBody,
			wantUpstream: 1,
		},
		{
			name:         "upstream content length stream",
			stream:       true,
			body:         shortBody,
			upstreamBody: contentLengthBody,
			wantUpstream: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &recordingKiroTempUnschedRepo{}
			upstream := &queuedHTTPUpstream{}
			if tt.upstreamBody != "" {
				upstream.responses = []*http.Response{
					newJSONResponse(http.StatusBadRequest, tt.upstreamBody),
				}
			}
			svc := newKiroPromptGateway(upstream, repo)
			account := kiroPromptLimitAccount(tt.maxInput, tt.limitModel)
			c, rec := kiroPromptGin()
			parsed := &ParsedRequest{
				Body:   NewRequestBodyRef(tt.body),
				Model:  "claude-sonnet-4-5",
				Stream: tt.stream,
			}

			_, err := svc.forwardKiroMessages(context.Background(), c, account, parsed, time.Now())
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			requireKiroPromptTooLongResponse(t, rec, tt.wantNumbers)
			require.Len(t, upstream.requests, tt.wantUpstream)
			require.False(t, repo.called)
			require.False(t, repo.rateCalled)
			rawEvents, _ := c.Get(OpsUpstreamErrorsKey)
			events, _ := rawEvents.([]*OpsUpstreamErrorEvent)
			require.NotEmpty(t, events)
			require.Equal(t, "http_error", events[0].Kind)
		})
	}
}

func TestForwardKiroMessagesUnknownTokenLimitDoesNotReject(t *testing.T) {
	body := kiroPromptAnthropicBody([]any{
		map[string]any{"role": "user", "content": "hello"},
	})
	cases := []struct {
		name  string
		limit int64
		model string
		req   string
	}{
		{name: "list not fetched", req: "claude-sonnet-4-5"},
		{name: "model not in list", limit: 1, model: "claude-opus-4-6", req: "claude-sonnet-4-5"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			repo := &recordingKiroTempUnschedRepo{}
			upstream := &queuedHTTPUpstream{responses: []*http.Response{
				newJSONResponse(http.StatusOK, `{"ok":true}`),
			}}
			svc := newKiroPromptGateway(upstream, repo)
			account := kiroPromptLimitAccount(tt.limit, tt.model)
			c, rec := kiroPromptGin()
			parsed := &ParsedRequest{
				Body:  NewRequestBodyRef(body),
				Model: tt.req,
			}

			_, err := svc.forwardKiroMessages(context.Background(), c, account, parsed, time.Now())
			require.Error(t, err)
			var tooLong *kiroPromptTooLongError
			require.NotErrorAs(t, err, &tooLong)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.NotEqual(t, http.StatusBadRequest, rec.Code)
			require.NotContains(t, rec.Body.String(), "prompt is too long")
			require.Len(t, upstream.requests, 1)
			require.False(t, repo.called)
			require.False(t, repo.rateCalled)
		})
	}
}

func TestHandleKiroHTTPErrorContentLengthDoesNotFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	account := &Account{
		ID:       9,
		Name:     "kiro",
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
	}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Models: map[string]UpstreamModelMetadata{
			"claude-sonnet-4-5": {ID: "claude-sonnet-4.5", ContextWindow: 1},
		},
	})
	repo := &recordingKiroTempUnschedRepo{}
	svc := &GatewayService{accountRepo: repo}
	resp := newJSONResponse(http.StatusBadRequest, `{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD","message":"Input is too long."}`)
	requestBody := kiroPromptAnthropicBody([]any{
		map[string]any{"role": "user", "content": "hello"},
	})

	err := svc.handleKiroHTTPError(context.Background(), resp, c, account, "claude-sonnet-4-5", requestBody)
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.NotErrorAs(t, err, &failover)
	require.False(t, repo.called)
	require.False(t, repo.rateCalled)
	requireKiroPromptTooLongResponse(t, rec, true)
}

func kiroPromptAnthropicBody(messages []any) []byte {
	raw, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 32,
		"messages":   messages,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func kiroPromptLimitAccount(maxInput int64, model string) *Account {
	account := &Account{
		ID:       77,
		Name:     "kiro-limit",
		Platform: PlatformKiro,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":    "ksk_test",
			"api_region": "us-east-1",
		},
	}
	if maxInput > 0 && strings.TrimSpace(model) != "" {
		account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
			Source: "kiro",
			Models: map[string]UpstreamModelMetadata{
				model: {
					ID:               model,
					ContextWindow:    maxInput,
					MaxContextWindow: maxInput,
					MaxOutputTokens:  64_000,
				},
			},
		})
	}
	return account
}

func newKiroPromptGateway(upstream *queuedHTTPUpstream, repo *recordingKiroTempUnschedRepo) *GatewayService {
	return &GatewayService{
		httpUpstream:        upstream,
		accountRepo:         repo,
		kiroCooldownStore:   &stubKiroCooldownStore{},
		tlsFPProfileService: &TLSFingerprintProfileService{},
	}
}

func kiroPromptGin() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func requireKiroPromptTooLongResponse(t *testing.T, rec *httptest.ResponseRecorder, wantNumbers bool) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	body := rec.Body.String()
	require.Equal(t, "error", gjson.Get(body, "type").String())
	require.Equal(t, "invalid_request_error", gjson.Get(body, "error.type").String())
	message := gjson.Get(body, "error.message").String()
	require.Truef(t, strings.HasPrefix(message, "prompt is too long"), "message=%q body=%s", message, body)
	if wantNumbers {
		require.Regexp(t, `^prompt is too long: \d+ tokens > 1 maximum$`, message)
	}
}
