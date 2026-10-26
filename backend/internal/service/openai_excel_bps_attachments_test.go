package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type nativeAttachmentUpstream struct {
	HTTPUpstream
	do func(*http.Request, string, int64, int) (*http.Response, error)
}

func (u *nativeAttachmentUpstream) Do(r *http.Request, p string, id int64, n int) (*http.Response, error) {
	return u.do(r, p, id, n)
}

type nativeAttachmentBody struct {
	io.Reader
	closed bool
}

func (b *nativeAttachmentBody) Close() error { b.closed = true; return nil }

func nativeGatewayBody(t *testing.T) ([]byte, []byte) {
	t.Helper()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	part := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes()), "detail": "original"}
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{part, part}}}})
	require.NoError(t, err)
	return body, data.Bytes()
}

func TestExcelBPSNativeAttachmentForward(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				body, pixels := nativeGatewayBody(t)
				body = bytes.Replace(body, []byte("{\"input\""), []byte(fmt.Sprintf("{\"stream\":%t,\"input\"", stream)), 1)
				uploaded := &nativeAttachmentBody{Reader: strings.NewReader("{\"openai_file_id\":\"file-native123\"}")}
				calls := 0
				svc := openAIClientToolsTestService(nil)
				account := excelAccount()
				account.Concurrency = 1
				account.Proxy = &Proxy{Protocol: "http", Host: "proxy.example", Port: 8080}
				svc.httpUpstream = &nativeAttachmentUpstream{do: func(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
					calls++
					require.Equal(t, account.ID, id)
					require.Equal(t, 1, concurrency)
					require.Equal(t, account.Proxy.URL(), proxy)
					require.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
					require.Equal(t, "test-account", req.Header.Get("Chatgpt-Account-Id"))
					require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
					require.Empty(t, req.Header.Get("Cookie"))
					require.Empty(t, req.Header.Get("x-codex-turn-state"))
					if calls == 1 {
						require.Equal(t, basispoints.AttachmentsURL, req.URL.String())
						require.Equal(t, "identity", req.Header.Get("Accept-Encoding"))
						deadline, ok := req.Context().Deadline()
						require.True(t, ok)
						require.LessOrEqual(t, time.Until(deadline), 60*time.Second)
						raw, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						require.Equal(t, int64(len(raw)), req.ContentLength)
						req.Body = io.NopCloser(bytes.NewReader(raw))
						mr, err := req.MultipartReader()
						require.NoError(t, err)
						part, err := mr.NextPart()
						require.NoError(t, err)
						require.Equal(t, "file", part.FormName())
						data, err := io.ReadAll(part)
						require.NoError(t, err)
						require.Equal(t, pixels, data)
						_, err = mr.NextPart()
						require.ErrorIs(t, err, io.EOF)
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: uploaded}, nil
					}
					require.True(t, uploaded.closed, "upload slot must be released before Responses at concurrency=1")
					require.Equal(t, basispoints.ResponsesURL, req.URL.String())
					raw, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					require.NotContains(t, string(raw), "data:image")
					found := 0
					for _, item := range gjson.GetBytes(raw, "input").Array() {
						for _, part := range item.Get("content").Array() {
							if part.Get("type").String() == "input_image" {
								found++
								require.Equal(t, "file-native123", part.Get("file_id").String())
								require.Len(t, part.Map(), 2, "native file references only accept type and file_id")
								require.False(t, part.Get("image_url").Exists())
							}
						}
					}
					require.Equal(t, 2, found)
					wire := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}, nil
				}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				c.Request.Header.Set("Cookie", "private-cookie")
				c.Request.Header.Set("x-codex-turn-state", "private-state")
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, calls)
			})
		}
	}
}

// Upload failures happen before any output. 429 cools only the BPS route,
// 401/402 disqualify the account, and 5xx/529 or transport failures move on
// (529 also marks the account overloaded); all of these fail over. A 403
// answers directly without touching the account (BPS may refuse attachments
// while text still works), and so does anything else.
func TestExcelBPSNativeUploadFailures(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		transport    bool
		wantReason   GatewayFailureReason
		wantClient   int
		want         int
		wantOverload bool
	}{
		{name: "429", status: 429, body: "PRIVATE_BODY", wantReason: ExcelBPSRateLimitedReason, wantClient: 429},
		{name: "401", status: 401, body: "PRIVATE_BODY", wantReason: ExcelBPSAccountRejectedReason, wantClient: 502},
		{name: "402", status: 402, body: "PRIVATE_BODY", wantReason: ExcelBPSAccountRejectedReason, wantClient: 502},
		{name: "500", status: 500, body: "PRIVATE_BODY", wantReason: ExcelBPSUpstreamErrorReason, wantClient: 502},
		{name: "529", status: 529, body: "PRIVATE_BODY", wantReason: ExcelBPSUpstreamErrorReason, wantClient: 503, wantOverload: true},
		{name: "transport", transport: true, wantReason: ExcelBPSUpstreamErrorReason, wantClient: 502},
		{name: "403", status: 403, body: "{\"error\":{\"code\":\"forbidden\",\"message\":\"PRIVATE_BODY\"}}", want: 403},
		{name: "413", status: 413, body: "PRIVATE_BODY", want: 413},
		{name: "redirect", status: 307, body: "PRIVATE_BODY", want: 502},
		{name: "wrong ID", status: 200, body: "{\"openai_file_id\":\"PRIVATE_BODY\"}", want: 502},
		{name: "invalid JSON", status: 200, body: "PRIVATE_BODY", want: 502},
		{name: "oversized JSON", status: 200, body: strings.Repeat("X", (64<<10)+1), want: 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := openAIClientToolsTestService(nil)
			repo := &excelBPSAccountStateRepo{}
			svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
			calls := 0
			closed := &nativeAttachmentBody{Reader: strings.NewReader(tc.body)}
			svc.httpUpstream = &nativeAttachmentUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				calls++
				require.Equal(t, basispoints.AttachmentsURL, req.URL.String())
				if tc.transport {
					return nil, errors.New("PRIVATE_TOKEN")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: closed}, nil
			}}
			body, _ := nativeGatewayBody(t)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			account := excelAccount()

			_, err := svc.Forward(context.Background(), c, account, body)

			require.Error(t, err)
			require.Equal(t, 1, calls, "the Responses request is never sent")
			require.NotContains(t, rec.Body.String(), "PRIVATE")
			require.NotContains(t, err.Error(), "PRIVATE")
			require.Equal(t, tc.status == http.StatusTooManyRequests, svc.isExcelBPSCoolingDown(account, "gpt-6-astra"))
			require.Equal(t, tc.wantOverload, len(repo.overloaded) > 0)
			if tc.wantReason != "" {
				if tc.status == http.StatusTooManyRequests {
					requireExcelBPSRateLimitFailover(t, err, c)
				}
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, tc.wantReason, failover.Reason)
				require.Equal(t, tc.wantClient, failover.ClientStatusCode)
				require.True(t, failover.ShouldRetryNextAccount())
				require.Zero(t, rec.Body.Len(), "leave the response to the handler")
			} else {
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
				require.Equal(t, tc.want, rec.Code)
				require.Equal(t, "basispoints_attachment_error", gjson.Get(rec.Body.String(), "error.code").String())
				require.True(t, IsResponseCommitted(c))
				require.Empty(t, repo.errors, "the account is not touched")
				require.Empty(t, repo.tempUnschedulable)
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			}
			if !tc.transport {
				require.True(t, closed.closed)
			}
		})
	}
}

// Images BPS cannot carry move to another account before any network call;
// other protocol errors still answer 400.
func TestExcelBPSNativePreflightBeforeNetwork(t *testing.T) {
	body, _ := nativeGatewayBody(t)
	for name, tc := range map[string]struct {
		change       func(t *testing.T, v map[string]any)
		imageFailure bool
	}{
		"invalid second image": {change: func(t *testing.T, v map[string]any) {
			parts := nativeGatewayParts(t, v)
			part, ok := parts[1].(map[string]any)
			require.True(t, ok)
			part["image_url"] = "data:image/png;base64,PRIVATE"
		}, imageFailure: true},
		"ambiguous reference": {change: func(t *testing.T, v map[string]any) {
			parts := nativeGatewayParts(t, v)
			part, ok := parts[0].(map[string]any)
			require.True(t, ok)
			part["file_id"] = "file-mixed"
		}, imageFailure: true},
		"unsupported URL": {change: func(t *testing.T, v map[string]any) {
			parts := nativeGatewayParts(t, v)
			part, ok := parts[0].(map[string]any)
			require.True(t, ok)
			part["image_url"] = "http://images.example/PRIVATE.png"
		}, imageFailure: true},
		"too many images": {change: func(t *testing.T, v map[string]any) {
			input, ok := v["input"].([]any)
			require.True(t, ok)
			message, ok := input[0].(map[string]any)
			require.True(t, ok)
			parts := nativeGatewayParts(t, v)
			for len(parts) <= 20 {
				parts = append(parts, parts[0])
			}
			message["content"] = parts
		}, imageFailure: true},
		"invalid tool choice": {change: func(_ *testing.T, v map[string]any) { v["tool_choice"] = "required" }},
		"previous response":   {change: func(_ *testing.T, v map[string]any) { v["previous_response_id"] = "resp_unsupported" }},
	} {
		t.Run(name, func(t *testing.T) {
			var source map[string]any
			require.NoError(t, json.Unmarshal(body, &source))
			tc.change(t, source)
			raw, err := json.Marshal(source)
			require.NoError(t, err)
			upstream := &httpUpstreamRecorder{}
			svc := openAIClientToolsTestService(upstream)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))

			_, err = svc.Forward(context.Background(), c, excelAccount(), raw)

			require.Error(t, err)
			require.Empty(t, upstream.requests)
			require.NotContains(t, rec.Body.String(), "PRIVATE")
			var failover *UpstreamFailoverError
			if tc.imageFailure {
				require.ErrorAs(t, err, &failover)
				require.Equal(t, ExcelBPSImageUnsupportedReason, failover.Reason)
				require.Equal(t, http.StatusBadRequest, failover.ClientStatusCode)
				require.True(t, failover.ShouldRetryNextAccount())
				require.False(t, failover.ShouldReportAccountScheduleFailure(), "the images, not the account, are the problem")
				require.NotContains(t, failover.ClientMessage, "PRIVATE")
				require.NotEmpty(t, failover.ClientMessage)
				require.Zero(t, rec.Body.Len(), "leave the response to the handler")
				return
			}
			require.NotErrorAs(t, err, &failover)
			require.Equal(t, 400, rec.Code)
		})
	}
}

// History images are uploaded too, and a conversation reuses its uploads.
func TestExcelBPSNativeAttachmentsReuseWithinConversation(t *testing.T) {
	body, _ := nativeGatewayBody(t)
	var source map[string]any
	require.NoError(t, json.Unmarshal(body, &source))
	input, ok := source["input"].([]any)
	require.True(t, ok)
	source["input"] = append(input,
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "seen"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "and again"}}})
	source["prompt_cache_key"] = "conversation-images"
	body, err := json.Marshal(source)
	require.NoError(t, err)
	svc := openAIClientToolsTestService(nil)
	uploads, responses := 0, 0
	svc.httpUpstream = &nativeAttachmentUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		if req.URL.String() == basispoints.AttachmentsURL {
			uploads++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-history"}`))}, nil
		}
		responses++
		raw, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "data:image")
		require.Contains(t, string(raw), `"file_id":"file-history"`)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_history\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	}}
	account := excelAccount()
	for turn := 0; turn < 2; turn++ {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		_, err := svc.Forward(context.Background(), c, account, body)
		require.NoError(t, err)
	}
	require.Equal(t, 2, responses)
	require.Equal(t, 1, uploads, "the second turn reuses the conversation's attachment")
}

func TestExcelBPSNativeAttachmentIDRedaction(t *testing.T) {
	raw := "{\"error\":{\"message\":\"cannot read file-private123 from this account\"}}"
	out := excelBPSSanitizeErrorBody(raw, "test-token", excelAccount())
	require.NotContains(t, out, "private123")
	require.Contains(t, out, "file-[redacted]")
}

func nativeGatewayParts(t *testing.T, v map[string]any) []any {
	t.Helper()
	input, ok := v["input"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, input)
	message, ok := input[0].(map[string]any)
	require.True(t, ok)
	parts, ok := message["content"].([]any)
	require.True(t, ok)
	return parts
}

func TestExcelBPSNativeToolScreenshotsStayInline(t *testing.T) {
	imageBody, _ := nativeGatewayBody(t)
	dataURL := gjson.GetBytes(imageBody, "input.0.content.0.image_url").String()
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		for _, mixed := range []bool{false, true} {
			for _, detail := range []string{"", "null", "low", "high", "original"} {
				t.Run(fmt.Sprintf("%s/mixed=%t/detail=%s", kind, mixed, detail), func(t *testing.T) {
					part := map[string]any{"type": "input_image", "image_url": dataURL}
					wantDetail := detail
					if detail == "null" {
						part["detail"] = nil
					} else if detail != "" {
						part["detail"] = detail
					}
					if detail == "" || detail == "null" {
						wantDetail = "auto"
					}
					input := []any{}
					if mixed {
						input = append(input, map[string]any{"role": "user", "content": []any{part}})
					}
					input = append(input, map[string]any{"type": "function_call", "name": "view_image", "call_id": "call_image", "arguments": "{}"}, map[string]any{"type": kind, "call_id": "call_image", "output": []any{part, map[string]any{"type": "input_text", "text": "screenshot context"}}})
					body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": input})
					require.NoError(t, err)
					svc := openAIClientToolsTestService(nil)
					uploads, responses := 0, 0
					svc.httpUpstream = &nativeAttachmentUpstream{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
						if req.URL.String() == basispoints.AttachmentsURL {
							uploads++
							require.True(t, mixed, "tool-only requests must not upload")
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-user-image"}`))}, nil
						}
						responses++
						raw, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						found := false
						for _, item := range gjson.GetBytes(raw, "input").Array() {
							if item.Get("type").String() == "function_call_output" {
								found = true
								require.Equal(t, dataURL, item.Get("output.0.image_url").String())
								require.False(t, item.Get("output.0.file_id").Exists())
								require.Equal(t, wantDetail, item.Get("output.0.detail").String())
								require.Equal(t, "screenshot context", item.Get("output.1.text").String())
							}
						}
						require.True(t, found)
						if mixed {
							require.Contains(t, string(raw), `"file_id":"file-user-image"`)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_screenshot\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
					}}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
					_, err = svc.Forward(context.Background(), c, excelAccount(), body)
					require.NoError(t, err)
					require.Equal(t, 1, responses)
					if mixed {
						require.Equal(t, 1, uploads)
					} else {
						require.Zero(t, uploads)
					}
				})
			}
		}
	}
}
