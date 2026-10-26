package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/fish"
)

func TestClassifyFishErrorAuth(t *testing.T) {
	failure := classifyFishError(&fish.AuthError{RequestError: fish.RequestError{Message: "Invalid token", StatusCode: 401}})
	if failure.Failover == nil || failure.Failover.Reason != fishFailureAuth {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if !failure.SetAccountError {
		t.Fatal("auth must set account error (fish token has no refresh)")
	}
	if failure.Cooldown > 0 {
		t.Fatal("auth should not cooldown")
	}
}

func TestClassifyFishErrorInsufficientCredits(t *testing.T) {
	failure := classifyFishError(&fish.InsufficientCreditsError{RequestError: fish.RequestError{Message: "insufficient credits", StatusCode: 402}})
	if failure.Failover == nil || failure.Failover.Reason != fishFailureInsufficientCredits {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if failure.Cooldown != 2*time.Hour {
		t.Fatalf("cooldown = %v, want 2h (quota class)", failure.Cooldown)
	}
	if failure.Failover.NextAccountAction != NextAccountRetry {
		t.Fatal("insufficient credits should retry next account")
	}
}

func TestClassifyFishErrorForbidden(t *testing.T) {
	native := `fish: GENERATION_UNAVAILABLE: {"code":"GENERATION_UNAVAILABLE"} (status 403)`
	failure := classifyFishError(&fish.ForbiddenError{RequestError: fish.RequestError{
		Message: `GENERATION_UNAVAILABLE: {"code":"GENERATION_UNAVAILABLE"}`, StatusCode: 403}})
	if failure.Failover == nil || failure.Failover.Reason != fishFailureForbidden {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if failure.Failover.NextAccountAction != NextAccountRetry {
		t.Fatal("forbidden should retry next account")
	}
	if failure.Failover.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", failure.Failover.StatusCode)
	}
	if !failure.SetAccountError {
		t.Fatal("forbidden must set account error (risk control)")
	}
	if failure.AccountErrorMessage != native {
		t.Fatalf("account error message = %q, want native %q", failure.AccountErrorMessage, native)
	}
	if failure.Cooldown > 0 {
		t.Fatal("forbidden should not cooldown (error state already unschedulable)")
	}
}

func TestClassifyFishErrorRateLimited(t *testing.T) {
	failure := classifyFishError(&fish.RateLimitedError{RequestError: fish.RequestError{StatusCode: 429}})
	if failure.Cooldown != fishRateLimitCooldown || failure.Failover.Reason != fishFailureUpstream {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestClassifyFishErrorTemporary(t *testing.T) {
	failure := classifyFishError(&fish.UpstreamTemporaryError{RequestError: fish.RequestError{StatusCode: 503}})
	if failure.Failover == nil || failure.Failover.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if failure.Failover.NextAccountAction != NextAccountRetry || failure.Cooldown > 0 {
		t.Fatalf("temporary should retry without side effects: %+v", failure)
	}
}

func TestClassifyFishErrorRequestStops(t *testing.T) {
	failure := classifyFishError(&fish.InvalidCombinationError{RequestError: fish.RequestError{Message: "Pricing configuration must match exactly one rate: matches=0", StatusCode: 400}})
	if failure.Failover == nil || failure.Failover.NextAccountAction != NextAccountStop {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if failure.Failover.Reason != fishFailureRequest {
		t.Fatalf("reason = %q", failure.Failover.Reason)
	}
	if failure.Failover.ClientStatusCode != http.StatusBadRequest {
		t.Fatalf("client status = %d", failure.Failover.ClientStatusCode)
	}
}

func TestClassifyFishErrorIndeterminate(t *testing.T) {
	failure := classifyFishError(&fishSubmitIndeterminateError{err: errors.New("conn reset")})
	if failure.Failover == nil || failure.Failover.NextAccountAction != NextAccountStop {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if failure.Failover.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d", failure.Failover.StatusCode)
	}
}

func TestClassifyFishErrorResultUnavailable(t *testing.T) {
	// 内层是 auth：账号侧处置保留（SetAccountError），但对本请求不换号
	failure := classifyFishError(&fishResultUnavailableError{
		groupID: "g",
		err:     &fish.AuthError{RequestError: fish.RequestError{StatusCode: 401}},
	})
	if failure.Failover == nil || failure.Failover.NextAccountAction != NextAccountStop {
		t.Fatalf("failover = %+v", failure.Failover)
	}
	if failure.Failover.Reason != fishFailureResultUnavailable {
		t.Fatalf("reason = %q", failure.Failover.Reason)
	}
	if !failure.SetAccountError {
		t.Fatal("inner auth side effect must be preserved")
	}
}

func TestClassifyFishErrorGenerationFailedRetries(t *testing.T) {
	failure := classifyFishError(&fishGenerationFailedError{groupID: "g", reason: "moderation"})
	if failure.Failover == nil || failure.Failover.NextAccountAction != NextAccountRetry {
		t.Fatalf("failover = %+v", failure.Failover)
	}
}

func TestClassifyFishErrorForeign(t *testing.T) {
	if failure := classifyFishError(errors.New("context canceled")); failure.Failover != nil {
		t.Fatalf("foreign error must yield nil failover, got %+v", failure.Failover)
	}
	if failure := classifyFishError(nil); failure.Failover != nil {
		t.Fatalf("nil must yield nil failover")
	}
}

type fishCooldownRepo struct {
	account *Account
	getErr  error
	until   time.Time
	reason  string
	gets    int
	sets    int
	// writeCtxErrs 记录每次写入时 ctx.Err()：真实库写入在已过期的 ctx 上会失败。
	writeCtxErrs []error
}

func (r *fishCooldownRepo) GetByID(context.Context, int64) (*Account, error) {
	r.gets++
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.account, nil
}

// SetTempUnschedulable 镜像仓储 SQL：只延长（temp_unschedulable_until < $1 才更新）。
func (r *fishCooldownRepo) SetTempUnschedulable(ctx context.Context, _ int64, until time.Time, reason string) error {
	r.sets++
	r.writeCtxErrs = append(r.writeCtxErrs, ctx.Err())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.until.Before(until) {
		r.until = until
		r.reason = reason
	}
	return nil
}

func (r *fishCooldownRepo) assertWritesHadLiveContext(t *testing.T) {
	t.Helper()
	for i, err := range r.writeCtxErrs {
		if err != nil {
			t.Fatalf("cooldown write %d ran on a dead context: %v", i, err)
		}
	}
}

func fishCooldownAccount() *Account {
	return &Account{
		ID:       7,
		Platform: PlatformFish,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			fishCredentialToken:    "tok",
			fishCredentialDeviceID: "dev-1",
		},
	}
}

func fishInsufficientFailure() fishFailure {
	return classifyFishError(&fish.InsufficientCreditsError{
		RequestError: fish.RequestError{Message: "insufficient credits", StatusCode: 402},
	})
}

// fishEntitlementServer 只提供 entitlements。其它路径记为 other，用来确认没打钱包。
func fishEntitlementServer(t *testing.T, status int, body string, block bool) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var entitlements, other atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/image-video/entitlements" {
			other.Add(1)
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		entitlements.Add(1)
		if block {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
				t.Error("entitlements request was not cancelled by the lookup timeout")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	orig := fishUsageAPIBaseForTest
	fishUsageAPIBaseForTest = server.URL
	t.Cleanup(func() { fishUsageAPIBaseForTest = orig })
	// 后台补查改为同步，断言时补查已完成。
	origAsync := fishRunAsync
	fishRunAsync = func(fn func()) { fn() }
	t.Cleanup(func() { fishRunAsync = origAsync })
	return server, &entitlements, &other
}

func assertFishDurationCooldown(t *testing.T, repo *fishCooldownRepo, started time.Time, want time.Duration, reason string) {
	t.Helper()
	repo.assertWritesHadLiveContext(t)
	if repo.sets != 1 {
		t.Fatalf("cooldown writes = %d, want 1", repo.sets)
	}
	if repo.reason != reason {
		t.Fatalf("reason = %q, want %q", repo.reason, reason)
	}
	if repo.until.Before(started.Add(want)) || repo.until.After(time.Now().Add(want)) {
		t.Fatalf("until = %v, want about %s after %v", repo.until, want, started)
	}
}

func TestFishInsufficientCooldownUsesDailyResetAt(t *testing.T) {
	_, entitlements, other := fishEntitlementServer(t, http.StatusOK,
		`{"plan":"free","dailyImage":{"limit":3,"used":3,"remaining":0,"resetAt":"2099-03-14T00:00:00Z"}}`, false)
	repo := &fishCooldownRepo{account: fishCooldownAccount()}
	applyFishCooldown(context.Background(), repo, 7, fishInsufficientFailure())

	// 先落 2h，再由补查延长到 resetAt。
	repo.assertWritesHadLiveContext(t)
	if repo.sets != 2 {
		t.Fatalf("cooldown writes = %d, want 2 (2h then resetAt)", repo.sets)
	}
	want := time.Date(2099, 3, 14, 0, 0, 0, 0, time.UTC)
	if !repo.until.Equal(want) {
		t.Fatalf("until = %v, want %v", repo.until, want)
	}
	if repo.reason != fishDailyQuotaExhaustedReason {
		t.Fatalf("reason = %q", repo.reason)
	}
	if entitlements.Load() != 1 || other.Load() != 0 {
		t.Fatalf("entitlements=%d other=%d, want 1 and 0", entitlements.Load(), other.Load())
	}
}

func TestFishInsufficientCooldownIgnoresCallerCancel(t *testing.T) {
	_, entitlements, _ := fishEntitlementServer(t, http.StatusOK,
		`{"plan":"free","dailyImage":{"limit":3,"used":3,"remaining":0,"resetAt":"2099-03-14T00:00:00Z"}}`, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := &fishCooldownRepo{account: fishCooldownAccount()}
	applyFishCooldown(ctx, repo, 7, fishInsufficientFailure())
	repo.assertWritesHadLiveContext(t)
	want := time.Date(2099, 3, 14, 0, 0, 0, 0, time.UTC)
	if !repo.until.Equal(want) {
		t.Fatalf("cancelled caller still needs resetAt, until = %v", repo.until)
	}
	if entitlements.Load() != 1 {
		t.Fatalf("entitlements hits = %d, want 1", entitlements.Load())
	}
}

func TestFishInsufficientCooldownKeepsTwoHoursWhenDailyRemains(t *testing.T) {
	_, entitlements, other := fishEntitlementServer(t, http.StatusOK,
		`{"plan":"free","dailyImage":{"limit":3,"used":1,"remaining":2,"resetAt":"2099-03-14T00:00:00Z"}}`, false)
	repo := &fishCooldownRepo{account: fishCooldownAccount()}
	started := time.Now()
	applyFishCooldown(context.Background(), repo, 7, fishInsufficientFailure())
	assertFishDurationCooldown(t, repo, started, fishInsufficientCooldown, "fish credits exhausted or quota exceeded")
	if entitlements.Load() != 1 || other.Load() != 0 {
		t.Fatalf("entitlements=%d other=%d, want 1 and 0", entitlements.Load(), other.Load())
	}
}

func TestFishInsufficientCooldownKeepsTwoHoursWhenLookupUnusable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		block  bool
	}{
		{
			name:   "upstream 5xx",
			status: http.StatusInternalServerError,
			body:   `{"message":"boom"}`,
		},
		{
			name:   "resetAt expired",
			status: http.StatusOK,
			body:   `{"plan":"free","dailyImage":{"limit":3,"used":3,"remaining":0,"resetAt":"2000-01-01T00:00:00Z"}}`,
		},
		{
			name:   "remaining omitted on a fresh window",
			status: http.StatusOK,
			body:   `{"plan":"free","dailyImage":{"limit":3,"used":0,"resetAt":"2099-03-14T00:00:00Z"}}`,
		},
		{
			// 钱包耗尽引起的 402：日窗口还有余量，只是 remaining 缺失被解成 0。
			name:   "remaining omitted with daily quota left",
			status: http.StatusOK,
			body:   `{"plan":"free","dailyImage":{"limit":100,"used":5,"resetAt":"2099-03-14T00:00:00Z"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fishEntitlementServer(t, tc.status, tc.body, tc.block)
			repo := &fishCooldownRepo{account: fishCooldownAccount()}
			started := time.Now()
			applyFishCooldown(context.Background(), repo, 7, fishInsufficientFailure())
			assertFishDurationCooldown(t, repo, started, fishInsufficientCooldown, "fish credits exhausted or quota exceeded")
		})
	}

	t.Run("lookup timeout", func(t *testing.T) {
		orig := fishDailyResetLookupTimeout
		fishDailyResetLookupTimeout = 30 * time.Millisecond
		t.Cleanup(func() { fishDailyResetLookupTimeout = orig })
		fishEntitlementServer(t, http.StatusOK, `{"dailyImage":{"limit":3,"used":3,"remaining":0,"resetAt":"2099-03-14T00:00:00Z"}}`, true)
		repo := &fishCooldownRepo{account: fishCooldownAccount()}
		started := time.Now()
		applyFishCooldown(context.Background(), repo, 7, fishInsufficientFailure())
		assertFishDurationCooldown(t, repo, started, fishInsufficientCooldown, "fish credits exhausted or quota exceeded")
	})
}

func TestFishRateLimitCooldownDoesNotQueryEntitlements(t *testing.T) {
	_, entitlements, other := fishEntitlementServer(t, http.StatusOK,
		`{"plan":"free","dailyImage":{"limit":3,"used":3,"remaining":0,"resetAt":"2099-03-14T00:00:00Z"}}`, false)
	repo := &fishCooldownRepo{account: fishCooldownAccount()}
	started := time.Now()
	failure := classifyFishError(&fish.RateLimitedError{RequestError: fish.RequestError{StatusCode: 429}})
	applyFishCooldown(context.Background(), repo, 7, failure)
	assertFishDurationCooldown(t, repo, started, fishRateLimitCooldown, "fish rate limited")
	if repo.gets != 0 || entitlements.Load() != 0 || other.Load() != 0 {
		t.Fatalf("gets=%d entitlements=%d other=%d, want no lookup", repo.gets, entitlements.Load(), other.Load())
	}
}
