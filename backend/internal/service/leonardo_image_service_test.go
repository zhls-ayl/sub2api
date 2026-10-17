package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/stretchr/testify/require"
)

func TestLeonardoAccountsServeGPTImageFamily(t *testing.T) {
	require.Equal(t, []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}, LeonardoModelIDs())

	native := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	relay := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://relay.example.com", "api_key": "sk-x",
	}}
	for name, account := range map[string]*Account{"native": native, "relay": relay} {
		require.True(t, account.IsModelSupported("gpt-image-2"), name)
		require.True(t, account.IsModelSupported("gpt-image-2.5-flare"), name)
		require.True(t, account.IsModelSupported("gpt-image-2.5-sunburst"), name)
		require.False(t, account.IsModelSupported("gpt-image-1"), name)
	}

	// 自定义映射绕不过平台口径：映射目标必须是已实现的模型。
	mapping := map[string]any{
		"my-image":    "gpt-image-2",
		"slug-image":  "openai/gpt-image-2.5-flare",
		"gpt-image-*": "gpt-image-9",
	}
	customNative := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{
		"cookie": "c", "model_mapping": mapping,
	}}
	customRelay := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://relay.example.com", "model_mapping": mapping,
	}}
	for name, account := range map[string]*Account{"native": customNative, "relay": customRelay} {
		require.True(t, account.IsModelSupported("my-image"), name)
		require.False(t, account.IsModelSupported("gpt-image-3"), name)
	}
	// 映射到上游 slug：Cookie 号可以（出图时本来就用 slug），中转号不行（OpenAI 兼容上游不认）。
	require.True(t, customNative.IsModelSupported("slug-image"))
	require.False(t, customRelay.IsModelSupported("slug-image"))

	// 缺 base_url 的 apikey 号既不是中转也没有原生链路：什么都不承接。
	bare := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-x"}}
	require.False(t, bare.IsModelSupported("gpt-image-2"))
}

func TestLeonardoGenerateCallRejectsUnsupportedUpstreamModel(t *testing.T) {
	svc := NewLeonardoImageService(nil)
	account := &Account{ID: 1, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{
		"cookie":        "c",
		"model_mapping": map[string]any{"gpt-image-2": "gpt-image-9"},
	}}
	_, err := svc.Generate(context.Background(), account, "jwt", &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "cat"})
	var requestErr *leonardopkg.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Contains(t, requestErr.User(), "not supported on Leonardo")
}

// moderation 接受但忽略（客户端常默认携带）；input_fidelity / style 仍明确拒绝。
func TestValidateLeonardoImageParamsIgnoresModerationAndInputFidelity(t *testing.T) {
	for _, moderation := range []string{"auto", "low"} {
		require.NoError(t, validateLeonardoImageParams(&OpenAIImagesRequest{Prompt: "cat", Moderation: moderation}), moderation)
	}
	for _, fidelity := range []string{"high", "low"} {
		require.NoError(t, validateLeonardoImageParams(&OpenAIImagesRequest{Prompt: "cat", InputFidelity: fidelity}), fidelity)
	}
	// input_fidelity 原生忽略，不需要转中转号。
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{InputFidelity: "high"}))

	err := validateLeonardoImageParams(&OpenAIImagesRequest{Prompt: "cat", Style: "vivid"})
	var requestErr *leonardopkg.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Contains(t, requestErr.User(), "style")
}

// 超出 OpenAI 像素上限的尺寸在提交前就以请求错误拒绝（上游校验会放行，
// 扣积分后生成阶段才 FAILED）。
func TestLeonardoGenerateRejectsOversizedImageBeforeSubmit(t *testing.T) {
	svc := NewLeonardoImageService(nil)
	account := &Account{ID: 1, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	_, err := svc.Generate(context.Background(), account, "jwt", &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "cat", Size: "3808x3584"})
	var requestErr *leonardopkg.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Contains(t, requestErr.User(), "total pixels")
}

func TestClassifyLeonardoResultUnavailableStopsFailover(t *testing.T) {
	failure := classifyLeonardoError(&leonardoResultUnavailableError{
		generationID: "gen-1",
		err:          leonardopkg.NewAuthError("jwt expired", http.StatusUnauthorized),
	})
	require.NotNil(t, failure.Failover)
	require.Equal(t, leonardoFailureResultUnavailable, failure.Failover.Reason)
	require.Equal(t, NextAccountStop, failure.Failover.NextAccountAction)
	require.False(t, failure.Failover.ShouldRetryNextAccount())
	require.Equal(t, http.StatusBadGateway, failure.Failover.ClientStatusCode)
	require.True(t, failure.InvalidateToken, "账号侧处置仍按内层错误执行")

	timeout := classifyLeonardoError(&leonardoResultUnavailableError{
		generationID: "gen-1",
		err:          fmt.Errorf("%w (last poll error: boom)", context.DeadlineExceeded),
	})
	require.NotNil(t, timeout.Failover)
	require.Equal(t, http.StatusGatewayTimeout, timeout.Failover.ClientStatusCode)
	require.False(t, timeout.Failover.ShouldRetryNextAccount())
}

func TestLeonardoStatusAliases(t *testing.T) {
	require.True(t, leonardopkg.IsCompleteStatus("COMPLETE"))
	require.True(t, leonardopkg.IsCompleteStatus("completed"))
	require.True(t, leonardopkg.IsFailedStatus("FAILED"))
	require.True(t, leonardopkg.IsFailedStatus("ERROR"))
	require.False(t, leonardopkg.IsCompleteStatus("PENDING"))
	require.False(t, leonardopkg.IsFailedStatus("UNKNOWN"))
}

// leonardoPollFakeUpstream 按 operationName 应答 GraphQL；handlers 未覆盖的操作返回 500。
func leonardoPollFakeUpstream(t *testing.T, handlers map[string]func() (int, string)) (*leonardopkg.Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OperationName string `json:"operationName"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		handler, ok := handlers[body.OperationName]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		status, payload := handler()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	client := leonardopkg.NewClient(leonardopkg.Options{GraphQLURL: server.URL, HTTPClient: server.Client()})
	return client, server.Close
}

func shortenLeonardoPollTimings(t *testing.T) {
	t.Helper()
	interval, backoff, grace := leonardoPollInterval, leonardoPollMaxBackoff, leonardoCompleteWithoutImagesGrace
	leonardoPollInterval = 5 * time.Millisecond
	leonardoPollMaxBackoff = 20 * time.Millisecond
	leonardoCompleteWithoutImagesGrace = 60 * time.Millisecond
	t.Cleanup(func() {
		leonardoPollInterval, leonardoPollMaxBackoff, leonardoCompleteWithoutImagesGrace = interval, backoff, grace
	})
}

const leonardoCompleteStatusBody = `{"data":{"generations":[{"id":"gen-1","status":"COMPLETE"}]}}`
const leonardoDetailWithImageBody = `{"data":{"generations":[{"id":"gen-1","status":"COMPLETE","imageWidth":1024,"imageHeight":1024,"generated_images":[{"id":"img-1","url":"https://cdn.example.com/1.jpg"}]}]}}`

func TestLeonardoPollRetriesTransientErrorsInPlace(t *testing.T) {
	shortenLeonardoPollTimings(t)
	var polls atomic.Int32
	client, closeServer := leonardoPollFakeUpstream(t, map[string]func() (int, string){
		"GetAIGenerationFeedStatuses": func() (int, string) {
			// 前两轮 5xx / 429：提交后不能因此换号重提，必须原地重试。
			switch polls.Add(1) {
			case 1:
				return http.StatusBadGateway, "bad gateway"
			case 2:
				return http.StatusTooManyRequests, "slow down"
			case 3:
				return http.StatusOK, `{"data":{"generations":[{"id":"gen-1","status":"PENDING"}]}}`
			default:
				return http.StatusOK, leonardoCompleteStatusBody
			}
		},
		"GetGenerationDetail": func() (int, string) { return http.StatusOK, leonardoDetailWithImageBody },
	})
	defer closeServer()

	detail, err := NewLeonardoImageService(nil).pollUntilDone(context.Background(), client, "jwt", "gen-1")
	require.NoError(t, err)
	require.Len(t, detail.Images, 1)
	require.GreaterOrEqual(t, polls.Load(), int32(4))
}

func TestLeonardoPollFailedStatusIsMarkedForFailover(t *testing.T) {
	shortenLeonardoPollTimings(t)
	client, closeServer := leonardoPollFakeUpstream(t, map[string]func() (int, string){
		"GetAIGenerationFeedStatuses": func() (int, string) {
			return http.StatusOK, `{"data":{"generations":[{"id":"gen-1","status":"ERROR"}]}}`
		},
		"GetGenerationFailureNotes": func() (int, string) {
			return http.StatusOK, `{"data":{"generations":[{"nsfw":false,"flagged":false,"notes":[{"failureReason":{"errorCode":"PROVIDER_MODERATION_ERROR"}}]}]}}`
		},
	})
	defer closeServer()

	_, err := NewLeonardoImageService(nil).pollUntilDone(context.Background(), client, "jwt", "gen-1")
	var failedErr *leonardoGenerationFailedError
	require.ErrorAs(t, err, &failedErr)
	var contentErr *leonardopkg.ContentRejectedError
	require.ErrorAs(t, failedErr.err, &contentErr)
}

func TestLeonardoPollAuthErrorIsNotRetried(t *testing.T) {
	shortenLeonardoPollTimings(t)
	var polls atomic.Int32
	client, closeServer := leonardoPollFakeUpstream(t, map[string]func() (int, string){
		"GetAIGenerationFeedStatuses": func() (int, string) {
			polls.Add(1)
			return http.StatusUnauthorized, "jwt expired"
		},
	})
	defer closeServer()

	_, err := NewLeonardoImageService(nil).pollUntilDone(context.Background(), client, "jwt", "gen-1")
	var authErr *leonardopkg.AuthError
	require.ErrorAs(t, err, &authErr)
	require.Equal(t, int32(1), polls.Load())
	var failedErr *leonardoGenerationFailedError
	require.False(t, errors.As(err, &failedErr), "鉴权失败不是上游 FAILED，GenerateCall 必须包成结果不可取")
}

func TestLeonardoPollWaitsForImagesAfterComplete(t *testing.T) {
	shortenLeonardoPollTimings(t)
	var details atomic.Int32
	client, closeServer := leonardoPollFakeUpstream(t, map[string]func() (int, string){
		"GetAIGenerationFeedStatuses": func() (int, string) { return http.StatusOK, leonardoCompleteStatusBody },
		"GetGenerationDetail": func() (int, string) {
			if details.Add(1) < 3 {
				return http.StatusOK, `{"data":{"generations":[{"id":"gen-1","status":"COMPLETE","generated_images":[{"id":"img-1","url":""}]}]}}`
			}
			return http.StatusOK, leonardoDetailWithImageBody
		},
	})
	defer closeServer()

	detail, err := NewLeonardoImageService(nil).pollUntilDone(context.Background(), client, "jwt", "gen-1")
	require.NoError(t, err)
	require.True(t, leonardoDetailHasImages(detail))
}

func TestLeonardoPollGivesUpWhenCompleteNeverHasImages(t *testing.T) {
	shortenLeonardoPollTimings(t)
	client, closeServer := leonardoPollFakeUpstream(t, map[string]func() (int, string){
		"GetAIGenerationFeedStatuses": func() (int, string) { return http.StatusOK, leonardoCompleteStatusBody },
		"GetGenerationDetail": func() (int, string) {
			return http.StatusOK, `{"data":{"generations":[{"id":"gen-1","status":"COMPLETE","generated_images":[]}]}}`
		},
	})
	defer closeServer()

	_, err := NewLeonardoImageService(nil).pollUntilDone(context.Background(), client, "jwt", "gen-1")
	var temporaryErr *leonardopkg.UpstreamTemporaryError
	require.ErrorAs(t, err, &temporaryErr)
}

func TestLeonardoPassiveUsageReadsCreditsSnapshot(t *testing.T) {
	svc := &AccountUsageService{cache: NewUsageCache()}
	native := &Account{ID: 71, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	svc.storeLeonardoUsageSnapshot(native.ID, &UsageInfo{
		LeonardoPlan:    "APPRENTICE",
		LeonardoCredits: &LeonardoCreditsSnapshot{Total: 8500, Subscription: 8000, Paid: 500},
	})

	usage, err := svc.getPassiveUsageForAccount(context.Background(), native)
	require.NoError(t, err)
	require.Equal(t, "passive", usage.Source)
	require.Equal(t, 8500, usage.LeonardoCredits.Total)

	relay := &Account{ID: 72, Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://relay.example.com"}}
	_, err = svc.getPassiveUsageForAccount(context.Background(), relay)
	require.Error(t, err)
}

func TestNewLeonardoCreditsSnapshotLimits(t *testing.T) {
	snapshot, name := newLeonardoCreditsSnapshot(&leonardopkg.Credits{
		Plan: "STANDARD", SubscriptionTokens: 24992, TotalTokens: 24992, UsableTokens: 24992,
	})
	require.Equal(t, "Premium", name)
	require.Equal(t, 25000, snapshot.FastLimit)
	require.Equal(t, 75000, snapshot.RolloverLimit)
	require.False(t, snapshot.FastDaily)

	// 剩余量超过常量表上限（额度上调 / 赠送）：以实际值为上限，进度条不溢出。
	boosted, _ := newLeonardoCreditsSnapshot(&leonardopkg.Credits{Plan: "BASIC", SubscriptionTokens: 9000, RolloverTokens: 30000})
	require.Equal(t, 9000, boosted.FastLimit)
	require.Equal(t, 30000, boosted.RolloverLimit)

	// FREE：Fast 按天发放，没有 Rollover Bank。
	free, freeName := newLeonardoCreditsSnapshot(&leonardopkg.Credits{Plan: "FREE", SubscriptionTokens: 150})
	require.Equal(t, "Free", freeName)
	require.True(t, free.FastDaily)
	require.Equal(t, 0, free.RolloverLimit)

	// 未知套餐：不推断上限，前端只显示数字。
	unknown, unknownName := newLeonardoCreditsSnapshot(&leonardopkg.Credits{Plan: "ENTERPRISE", SubscriptionTokens: 5})
	require.Empty(t, unknownName)
	require.Zero(t, unknown.FastLimit)
	require.Zero(t, unknown.RolloverLimit)
}
