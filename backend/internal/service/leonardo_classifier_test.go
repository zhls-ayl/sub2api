package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/stretchr/testify/require"
)

func TestClassifyLeonardoInsufficientTokens(t *testing.T) {
	failure := classifyLeonardoError(leonardopkg.NewInsufficientTokensError("insufficient tokens"))
	require.NotNil(t, failure.Failover)
	require.Equal(t, leonardoFailureInsufficientTokens, failure.Failover.Reason)
	require.Equal(t, GatewayFailureScopeAccount, failure.Failover.Scope)
	require.True(t, failure.Failover.ShouldRetryNextAccount())
	require.Equal(t, leonardoInsufficientCooldown, failure.Cooldown)
}

func TestClassifyLeonardoAuth(t *testing.T) {
	failure := classifyLeonardoError(leonardopkg.NewAuthError("jwt invalid", http.StatusUnauthorized))
	require.NotNil(t, failure.Failover)
	require.Equal(t, leonardoFailureAuth, failure.Failover.Reason)
	require.True(t, failure.InvalidateToken)
	require.True(t, failure.Failover.ShouldRetryNextAccount())
	require.Equal(t, time.Duration(0), failure.Cooldown)
}

func TestClassifyLeonardoSuspensionSetsAccountError(t *testing.T) {
	failure := classifyLeonardoError(leonardopkg.NewSuspensionError("you are suspended for violating our user agreement"))
	require.NotNil(t, failure.Failover)
	require.Equal(t, leonardoFailureSuspended, failure.Failover.Reason)
	require.True(t, failure.SetAccountError)
	require.NotEmpty(t, failure.AccountErrorMessage)
}

func TestClassifyLeonardoRateLimited(t *testing.T) {
	failure := classifyLeonardoError(leonardopkg.NewRateLimitedError("rate limited", http.StatusTooManyRequests))
	require.NotNil(t, failure.Failover)
	require.Equal(t, GatewayFailureScopeProvider, failure.Failover.Scope)
	require.Equal(t, leonardoRateLimitCooldown, failure.Cooldown)
}

func TestClassifyLeonardoContentRejectedIsRequestScoped(t *testing.T) {
	failure := classifyLeonardoError(leonardopkg.NewContentRejectedError("init image moderation rejected"))
	require.NotNil(t, failure.Failover)
	require.Equal(t, GatewayFailureScopeRequest, failure.Failover.Scope)
	require.True(t, IsLeonardoContentRejected(failure.Failover))
}

func TestClassifyLeonardoRequestErrorStopsFailover(t *testing.T) {
	failure := classifyLeonardoError(leonardopkg.NewRequestError("unsupported quality"))
	require.NotNil(t, failure.Failover)
	require.Equal(t, NextAccountStop, failure.Failover.NextAccountAction)
	require.False(t, failure.Failover.ShouldRetryNextAccount())
}

// 上游尺寸校验失败（宽高不在允许值清单内）：400、不换号，清单原文回给客户端。
func TestClassifyLeonardoSizeValidationErrorReturnsAllowedValues(t *testing.T) {
	const message = "An error occurred. (VALIDATION_ERROR: parameters.width must be one of: 768, 1024; parameters.height must be one of: 640, 1024)"
	failure := classifyLeonardoError(&leonardopkg.RequestError{Message: message})
	require.NotNil(t, failure.Failover)
	require.Equal(t, http.StatusBadRequest, failure.Failover.ClientStatusCode)
	require.Equal(t, NextAccountStop, failure.Failover.NextAccountAction)
	require.Contains(t, failure.Failover.ClientMessage, "parameters.width must be one of: 768, 1024")
}

func TestClassifyLeonardoIndeterminateStopsFailover(t *testing.T) {
	networkErr := &leonardopkg.RequestError{Message: "submit timeout", ErrorType: leonardopkg.ErrorTypeTimeout}
	failure := classifyLeonardoError(&leonardoSubmitIndeterminateError{err: networkErr})
	require.NotNil(t, failure.Failover)
	require.Equal(t, leonardoFailureIndeterminate, failure.Failover.Reason)
	require.Equal(t, NextAccountStop, failure.Failover.NextAccountAction)
	require.True(t, IsLeonardoIndeterminate(failure.Failover))
	require.Equal(t, http.StatusGatewayTimeout, failure.Failover.ClientStatusCode)
}

func TestClassifyLeonardoForeignErrorReturnsZero(t *testing.T) {
	require.Equal(t, leonardoFailure{}, classifyLeonardoError(errors.New("context canceled")))
	require.Equal(t, leonardoFailure{}, classifyLeonardoError(nil))
}

func TestLeonardoRelayPredicates(t *testing.T) {
	native := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{
		"cookie": "__Secure-better-auth.session_token=x",
	}}
	relay := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"base_url": "https://api.openai.com",
		"api_key":  "sk-x",
	}}
	relayWithoutBaseURL := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "sk-x",
	}}

	require.False(t, IsLeonardoRelayAccount(native))
	require.True(t, IsLeonardoRelayAccount(relay))
	require.False(t, IsLeonardoRelayAccount(relayWithoutBaseURL), "apikey 无 base_url 不是中转（本渠道不支持 Leonardo 官方 API Key）")

	// 原生号与中转号共用默认映射：对外名恒等（原生号出图时再换成上游 slug）。
	wantMapping := map[string]string{
		"gpt-image-2":            "gpt-image-2",
		"gpt-image-2.5-flare":    "gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst": "gpt-image-2.5-sunburst",
	}
	require.Equal(t, wantMapping, native.GetModelMapping())
	require.Equal(t, wantMapping, relay.GetModelMapping())

	// 中转号借用 OpenAI 转发链路：base_url / api_key 挂钩生效。
	require.Equal(t, "https://api.openai.com", relay.GetOpenAIBaseURL())
	require.Equal(t, "sk-x", relay.GetOpenAIProtocolAPIKey())
	require.Empty(t, native.GetOpenAIBaseURL())

	// RequiresRelay：mask / n>8 只走中转；透明背景、n≤8、stream（假流式）原生支持。
	require.True(t, LeonardoRequiresRelay(&OpenAIImagesRequest{HasMask: true}))
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{Background: "transparent"}))
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{N: 2}))
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{N: 8}))
	require.True(t, LeonardoRequiresRelay(&OpenAIImagesRequest{N: 9}))
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{Stream: true}), "stream 在原生号上走假流式")
	// output_format：png / jpeg 原生本机转码，webp 只走中转。
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{OutputFormat: "png"}))
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{OutputFormat: "jpeg"}))
	require.True(t, LeonardoRequiresRelay(&OpenAIImagesRequest{OutputFormat: " WebP "}))
	require.False(t, LeonardoRequiresRelay(&OpenAIImagesRequest{N: 1}))
	require.False(t, LeonardoRequiresRelay(nil))

	// 内容拒绝后跳过原生号的谓词。
	require.True(t, shouldSkipLeonardoNativeAccount(true, native))
	require.False(t, shouldSkipLeonardoNativeAccount(true, relay))
	require.False(t, shouldSkipLeonardoNativeAccount(false, native))
}

func TestLeonardoSelectionExclusions(t *testing.T) {
	failed := map[int64]struct{}{1: {}}
	extra := map[int64]struct{}{2: {}}
	merged := LeonardoRelaySelectionExclusions(failed, true, extra)
	require.Contains(t, merged, int64(1))
	require.Contains(t, merged, int64(2))

	// requireRelay=false 时不并入 extra，failed 保持只读。
	merged = LeonardoRelaySelectionExclusions(failed, false, extra)
	require.Contains(t, merged, int64(1))
	require.NotContains(t, merged, int64(2))
	require.NotContains(t, failed, int64(2))
}

func TestLeonardoTokenExpiredHelpers(t *testing.T) {
	// 无 token：需要刷新。
	require.True(t, leonardoTokenExpired(map[string]any{}, time.Minute))
	// token 存在但缺 expires_at：需要刷新。
	require.True(t, leonardoTokenExpired(map[string]any{"access_token": "jwt"}, time.Minute))
	// 未来 10 分钟过期：5 分钟窗口内不刷、15 分钟窗口内要刷。
	creds := map[string]any{
		"access_token": "jwt",
		"expires_at":   time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
	require.False(t, leonardoTokenExpired(creds, 5*time.Minute))
	require.True(t, leonardoTokenExpired(creds, 15*time.Minute))
}

func TestLeonardoRefresherEligibility(t *testing.T) {
	refresher := NewLeonardoTokenRefresher()
	cookie := "__Secure-better-auth.session_token=value"
	oauth := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": cookie}}
	apikey := &Account{Platform: PlatformLeonardo, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk"}}

	require.True(t, refresher.CanRefresh(oauth))
	require.False(t, refresher.CanRefresh(apikey))
	// 无 token：需要刷新。
	require.True(t, refresher.NeedsRefresh(oauth, time.Minute))
	require.NotEmpty(t, refresher.CacheKey(oauth))

	withToken := &Account{Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{
		"cookie":       cookie,
		"access_token": "jwt",
		"expires_at":   time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
	}}
	require.False(t, refresher.NeedsRefresh(withToken, time.Minute))
}

func TestLeonardoCookieSanitizePolicy(t *testing.T) {
	creds := map[string]any{"cookie": "x", "password": "y"}
	kept := SanitizeStoredCredentials(PlatformLeonardo, creds)
	require.Contains(t, kept, "cookie", "Leonardo 的 cookie 是长期凭据，必须保留")
	require.NotContains(t, kept, "password")

	stripped := SanitizeStoredCredentials(PlatformOpenAI, map[string]any{"cookie": "x"})
	require.NotContains(t, stripped, "cookie", "其它平台照旧剥离 cookie")
}

func TestLeonardoDefaultModelsListCandidates(t *testing.T) {
	require.Equal(t, LeonardoModelIDs(), defaultModelsListCandidateIDs(PlatformLeonardo))
	require.True(t, defaultAllowImageGenerationForPlatform(PlatformLeonardo))
}

func TestLeonardoCredentialFieldsSubset(t *testing.T) {
	full := map[string]any{
		"cookie":        "c",
		"access_token":  "j",
		"expires_at":    "2030",
		"cognito_sub":   "s",
		"model_mapping": map[string]any{"gpt-image-2": "gpt-image-2"},
	}
	fields := leonardoTokenCredentialFields(full)
	require.Contains(t, fields, "access_token")
	require.Contains(t, fields, "expires_at")
	require.Contains(t, fields, "cognito_sub")
	require.Contains(t, fields, "cookie", "轮转后的新 cookie 属于交换产物")
	require.NotContains(t, fields, "model_mapping", "管理员维护的字段不随 token 刷新写回")
}

func TestClassifyLeonardoProviderModerationIsRetryableRejection(t *testing.T) {
	providerErr := leonardopkg.ClassifyFailureErrorCode(&leonardopkg.GenerationFailure{
		ErrorCode:     "PROVIDER_MODERATION_ERROR",
		ClientSummary: "Content flagged by provider moderation: rejected.",
	})
	failure := classifyLeonardoError(providerErr)
	require.NotNil(t, failure.Failover)
	require.Equal(t, leonardoFailureProviderModeration, failure.Failover.Reason)
	require.True(t, IsLeonardoProviderModeration(failure.Failover))
	require.True(t, IsLeonardoContentRejected(failure.Failover))
	require.Equal(t, "Content flagged by provider moderation: rejected.", failure.Failover.ClientMessage)

	// Leonardo 自身的 NSFW 拦截是确定性的：仍是普通内容拒绝，不可重试。
	nsfw := classifyLeonardoError(leonardopkg.ClassifyFailureErrorCode(&leonardopkg.GenerationFailure{ErrorCode: "CC_NSFW_TOTAL_FAILURE"}))
	require.Equal(t, leonardoFailureContentRejected, nsfw.Failover.Reason)
	require.False(t, IsLeonardoProviderModeration(nsfw.Failover))
	require.True(t, IsLeonardoContentRejected(nsfw.Failover))
}
