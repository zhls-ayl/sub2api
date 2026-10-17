package leonardo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient 返回端点指向 test server 的 Client。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(Options{
		HTTPClient: server.Client(),
		SessionURL: server.URL + "/api/auth/get-session",
		GraphQLURL: server.URL + "/v1/graphql",
	})
	return client, server
}

func TestRefreshSessionSuccessWithRotation(t *testing.T) {
	var gotCookie string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Add("Set-Cookie", SessionTokenCookieName+"=rotated; Path=/")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session": map[string]any{"token": "h.eyJzdWIiOiJzdWItMSIsImVtYWlsIjoiYUBiLmMiLCJleHAiOjE4MDAwMDAwMDB9.s"},
		})
	})
	exchange, err := client.RefreshSession(context.Background(), "original-token-value")
	if err != nil {
		t.Fatalf("RefreshSession error: %v", err)
	}
	if !strings.Contains(gotCookie, SessionTokenCookieName+"=original-token-value") {
		t.Fatalf("request cookie = %q", gotCookie)
	}
	if exchange.JWT != "h.eyJzdWIiOiJzdWItMSIsImVtYWlsIjoiYUBiLmMiLCJleHAiOjE4MDAwMDAwMDB9.s" {
		t.Fatalf("jwt = %q", exchange.JWT)
	}
	if !exchange.CookieRotated {
		t.Fatal("rotation should be reported")
	}
	if !strings.Contains(exchange.RotatedCookie, SessionTokenCookieName+"=rotated") {
		t.Fatalf("rotated cookie = %q", exchange.RotatedCookie)
	}
}

func TestRefreshSessionNoJWT(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"user": map[string]any{}}})
	})
	if _, err := client.RefreshSession(context.Background(), "k=v"); err == nil {
		t.Fatal("expected no-JWT error")
	}
}

func TestRefreshSessionRateLimited(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := client.RefreshSession(context.Background(), "k=v")
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("429 should classify as RateLimitedError, got %T: %v", err, err)
	}
}

func TestRefreshSessionUnauthorized(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	_, err := client.RefreshSession(context.Background(), "k=v")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("401 should classify as AuthError, got %T: %v", err, err)
	}
}

func TestGraphQLInsufficientTokens(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Leo-Schema-Version") != "latest" {
			t.Errorf("schema version header = %q", r.Header.Get("X-Leo-Schema-Version"))
		}
		if r.Header.Get("Authorization") != "Bearer jwt-1" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": "insufficient tokens for model"}},
		})
	})
	_ = server
	_, err := client.QueryCredits(context.Background(), "jwt-1", "sub-1")
	var insufficient *InsufficientTokensError
	if !errors.As(err, &insufficient) {
		t.Fatalf("insufficient tokens should classify, got %T: %v", err, err)
	}
}

func TestSubmitGenerationParsesResult(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.OperationName != "Generate" {
			t.Errorf("operation = %q", req.OperationName)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"generate": map[string]any{"generationId": "gen-1", "apiCreditCost": 65},
			},
		})
	})
	result, err := client.SubmitGeneration(context.Background(), "jwt-1", GenerateRequest{Model: "gpt-image-2"})
	if err != nil {
		t.Fatalf("SubmitGeneration error: %v", err)
	}
	if result.GenerationID != "gen-1" || result.APICreditCost != 65 {
		t.Fatalf("result = %+v", result)
	}
}

// 参数校验错误：顶层 message 只有 "An error occurred."，宽高允许值清单在
// extensions.details 里（2026-10 实测响应体），要带进错误信息。
func TestSubmitGenerationValidationErrorKeepsDetails(t *testing.T) {
	const detail = "parameters.width must be one of: 768, 832, 1024, 1536; parameters.height must be one of: 640, 1024, 1536"
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": nil,
			"errors": []map[string]any{{
				"message": "An error occurred.",
				"extensions": map[string]any{
					"code": "BadRequestException",
					"details": map[string]any{
						"code":    "VALIDATION_ERROR",
						"errors":  []map[string]any{{"code": "VALIDATION_ERROR", "message": detail}},
						"message": detail,
					},
					"statusCode": 400,
				},
				"locations": []any{},
				"path":      []any{},
			}},
		})
	})
	_, err := client.SubmitGeneration(context.Background(), "jwt-1", GenerateRequest{Model: "gpt-image-2"})
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("validation error should be a RequestError, got %T: %v", err, err)
	}
	if !strings.Contains(reqErr.User(), "VALIDATION_ERROR: "+detail) {
		t.Fatalf("message = %q, want it to carry the details", reqErr.User())
	}
}

func TestGraphQLErrorMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		details string
		want    string
	}{
		{"no details", "insufficient tokens for model", "", "insufficient tokens for model"},
		{"details appended", "An error occurred.", `{"code":"VALIDATION_ERROR","message":"bad width"}`, "An error occurred. (VALIDATION_ERROR: bad width)"},
		{"details without code", "An error occurred.", `{"message":"bad width"}`, "An error occurred. (bad width)"},
		{"details same as message", "bad width", `{"message":"bad width"}`, "bad width"},
		{"details only", "", `{"code":"X","message":"bad width"}`, "X: bad width"},
		{"non-object details", "boom", `"just a string"`, "boom"},
		{"empty details message", "boom", `{"code":"X"}`, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.details != "" {
				raw = json.RawMessage(tc.details)
			}
			if got := graphQLErrorMessage(tc.message, raw); got != tc.want {
				t.Fatalf("graphQLErrorMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

// gpt-image-2.5 系列：apiCreditCost 为 null，实耗在 cost.amount（字符串）里。
func TestSubmitGenerationFallsBackToCostAmount(t *testing.T) {
	for _, generate := range []map[string]any{
		{"generationId": "gen-1", "apiCreditCost": nil, "cost": map[string]any{"amount": "137", "unit": "CREDITS"}},
		{"generationId": "gen-1", "apiCreditCost": nil, "cost": map[string]any{"amount": 137, "unit": "CREDITS"}},
	} {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"generate": generate}})
		})
		result, err := client.SubmitGeneration(context.Background(), "jwt-1", GenerateRequest{Model: "openai/gpt-image-2.5-sunburst"})
		if err != nil {
			t.Fatalf("SubmitGeneration error: %v", err)
		}
		if result.APICreditCost != 137 || result.CostUnit != "CREDITS" {
			t.Fatalf("result = %+v, want cost 137 CREDITS", result)
		}
	}
}

// 失败原因：当前 schema 的 failureReason 是 JSON 值，按 JSON 值写法一次查询命中。
func TestFailureReasonParsesCapturedNotes(t *testing.T) {
	cases := []struct {
		name        string
		generation  map[string]any
		wantCode    string
		wantSummary string
		wantRetry   bool
	}{
		{
			name: "provider moderation",
			generation: map[string]any{"nsfw": false, "flagged": false, "notes": []map[string]any{{
				"noteType": "PROVIDER_FAILURE",
				"failureReason": map[string]any{
					"errorCode":     "PROVIDER_MODERATION_ERROR",
					"clientSummary": "Content flagged by provider moderation: Your request was rejected by the safety system.",
				},
			}}},
			wantCode:    "PROVIDER_MODERATION_ERROR",
			wantSummary: "Content flagged by provider moderation: Your request was rejected by the safety system.",
			wantRetry:   true,
		},
		{
			name: "leonardo nsfw",
			generation: map[string]any{"nsfw": true, "flagged": false, "notes": []map[string]any{{
				"noteType":      "CC_NSFW_TOTAL_FAILURE",
				"failureReason": nil,
				"notePayload":   map[string]any{"failedImages": 1, "tokensRefunded": 8},
			}}},
			wantCode: "CC_NSFW_TOTAL_FAILURE",
		},
	}
	for _, tc := range cases {
		queries := 0
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			var req graphqlRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			queries++
			if strings.Contains(req.Query, "errorCode") {
				t.Errorf("%s: object-form query should not be needed", tc.name)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"generations": []map[string]any{tc.generation}}})
		})
		failure, err := client.FailureReason(context.Background(), "jwt-1", "gen-1")
		if err != nil {
			t.Fatalf("%s: FailureReason error: %v", tc.name, err)
		}
		if queries != 1 || failure.ErrorCode != tc.wantCode || failure.ClientSummary != tc.wantSummary {
			t.Fatalf("%s: queries=%d failure=%+v", tc.name, queries, failure)
		}

		var rejected *ContentRejectedError
		if err := ClassifyFailureErrorCode(failure); !errors.As(err, &rejected) {
			t.Fatalf("%s: want ContentRejectedError, got %T", tc.name, err)
		}
		if rejected.ProviderModeration != tc.wantRetry {
			t.Fatalf("%s: ProviderModeration = %v, want %v", tc.name, rejected.ProviderModeration, tc.wantRetry)
		}
		if tc.wantSummary != "" && rejected.User() != tc.wantSummary {
			t.Fatalf("%s: user message = %q", tc.name, rejected.User())
		}
	}
}

func TestPollGenerationStatus(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"generations": []map[string]any{{"id": "gen-1", "status": "COMPLETE"}},
			},
		})
	})
	status, err := client.PollGenerationStatus(context.Background(), "jwt-1", "gen-1")
	if err != nil {
		t.Fatalf("PollGenerationStatus error: %v", err)
	}
	if status.Status != StatusComplete {
		t.Fatalf("status = %q", status.Status)
	}
}

func TestPollGenerationStatusUnknownForMissingRow(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"generations": []any{}}})
	})
	status, err := client.PollGenerationStatus(context.Background(), "jwt-1", "gen-404")
	if err != nil {
		t.Fatalf("PollGenerationStatus error: %v", err)
	}
	if status.Status != StatusUnknown {
		t.Fatalf("status = %q", status.Status)
	}
}

func TestUploadInitImageAndModeration(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.OperationName {
		case "UploadImage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"uploadImage": map[string]any{
						"uploadId": "up-1",
						"url":      "https://s3.example/upload",
						"fields":   `{"key":"abc","policy":"xyz"}`,
					},
				},
			})
		case "GetInitImageModeration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"init_image_moderation": []map[string]any{{"akUUID": "up-1", "initImageId": "init-1", "checkStatus": "Accepted"}},
				},
			})
		default:
			t.Errorf("unexpected operation %q", req.OperationName)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	init, err := client.UploadInitImage(context.Background(), "jwt-1", "png")
	if err != nil {
		t.Fatalf("UploadInitImage error: %v", err)
	}
	if init.UploadID != "up-1" {
		t.Fatalf("uploadId = %q", init.UploadID)
	}
	id, err := client.WaitForInitImage(context.Background(), "jwt-1", "up-1", 0)
	if err != nil {
		t.Fatalf("WaitForInitImage error: %v", err)
	}
	if id != "init-1" {
		t.Fatalf("initImageId = %q", id)
	}
}
