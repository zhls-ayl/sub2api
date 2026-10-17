package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
)

// Leonardo 积分拉取的缓存与超时参数（与 Kiro 同构）。
const (
	leonardoUsageCacheTTL            = 3 * time.Minute
	leonardoUsageErrorTTL            = 1 * time.Minute
	leonardoUsageFetchTimeout        = 60 * time.Second
	leonardoCreditsExhaustedCooldown = 30 * time.Minute
)

// LeonardoCreditsSnapshot 是 Leonardo 账号的积分余额快照。上游只给各池剩余量；
// 上限来自套餐常量表（leonardo.LookupPlan），未知套餐时上限为 0（只展示数字）。
type LeonardoCreditsSnapshot struct {
	Total        int `json:"total"`
	Usable       int `json:"usable"`
	Subscription int `json:"subscription"`
	Paid         int `json:"paid"`
	Rollover     int `json:"rollover"`
	// FastLimit / RolloverLimit 是 Fast Tokens 与 Rollover Bank 的上限。
	FastLimit     int `json:"fast_limit"`
	RolloverLimit int `json:"rollover_limit"`
	// FastDaily 为 true 时 Fast Tokens 按天发放（FREE 套餐）。
	FastDaily bool `json:"fast_daily,omitempty"`
}

// newLeonardoCreditsSnapshot 由积分查询结果构造快照，并返回套餐展示名
// （未知套餐时为空）。剩余量超过常量表上限时（额度上调、赠送）以实际值为上限，
// 避免进度条溢出。
func newLeonardoCreditsSnapshot(credits *leonardopkg.Credits) (*LeonardoCreditsSnapshot, string) {
	snapshot := &LeonardoCreditsSnapshot{
		Total:        credits.TotalTokens,
		Usable:       credits.UsableTokens,
		Subscription: credits.SubscriptionTokens,
		Paid:         credits.PaidTokens,
		Rollover:     credits.RolloverTokens,
	}
	quota, ok := leonardopkg.LookupPlan(credits.Plan)
	if !ok {
		return snapshot, ""
	}
	snapshot.FastLimit = max(quota.FastTokens, credits.SubscriptionTokens)
	snapshot.RolloverLimit = max(quota.MaxRolloverTokens, credits.RolloverTokens)
	snapshot.FastDaily = quota.DailyFast
	return snapshot, quota.DisplayName
}

// leonardoUsageCache 缓存 Leonardo 积分快照，与 kiroUsageCache 同构。
type leonardoUsageCache struct {
	usageInfo *UsageInfo
	timestamp time.Time
}

func leonardoUsageCacheTTLFor(info *UsageInfo) time.Duration {
	if info != nil && info.Error != "" {
		return leonardoUsageErrorTTL
	}
	return leonardoUsageCacheTTL
}

func (s *AccountUsageService) getCachedLeonardoUsage(accountID int64) (*UsageInfo, bool) {
	cached, ok := s.cache.leonardoUsageCache.Load(accountID)
	if !ok {
		return nil, false
	}
	entry, ok := cached.(*leonardoUsageCache)
	if !ok || entry.usageInfo == nil {
		return nil, false
	}
	if time.Since(entry.timestamp) >= leonardoUsageCacheTTLFor(entry.usageInfo) {
		return nil, false
	}
	return cloneUsageInfo(entry.usageInfo), true
}

func (s *AccountUsageService) storeLeonardoUsageSnapshot(accountID int64, info *UsageInfo) {
	if info == nil {
		return
	}
	s.cache.leonardoUsageCache.Store(accountID, &leonardoUsageCache{
		usageInfo: cloneUsageInfo(info),
		timestamp: time.Now(),
	})
}

// getLeonardoUsage 拉取 Leonardo 账号的积分余额，写进 UsageInfo。
// cache + singleflight + 错误降级，与 getKiroUsage 同结构。余额归零时同步
// SetTempUnschedulable——比请求路径撞 insufficient tokens 再冷却更精准。
func (s *AccountUsageService) getLeonardoUsage(ctx context.Context, account *Account, source string, forceRefresh bool) (*UsageInfo, error) {
	now := time.Now()
	if account == nil {
		return &UsageInfo{
			Source:    source,
			UpdatedAt: &now,
			Error:     "account is nil",
			ErrorCode: errorCodeNetworkError,
		}, nil
	}
	if isLeonardoRelayAccount(account) {
		return &UsageInfo{Source: source, UpdatedAt: &now}, nil
	}

	cached, hasCached := s.getCachedLeonardoUsage(account.ID)
	if !forceRefresh && hasCached {
		cached.Source = source
		return cached, nil
	}

	flightKey := fmt.Sprintf("leonardo-usage:%d", account.ID)
	// 共享的上游调用脱离发起者的 ctx：首个调用方取消不能让其它等待者一起失败。
	resultCh := s.cache.leonardoUsageFlight.DoChan(flightKey, func() (any, error) {
		if !forceRefresh {
			if usage, ok := s.getCachedLeonardoUsage(account.ID); ok {
				return usage, nil
			}
		}
		sharedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leonardoUsageFetchTimeout)
		defer cancel()
		return s.fetchAndCacheLeonardoUsage(sharedCtx, account, source)
	})
	var result any
	var fetchErr error
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case flight := <-resultCh:
		result, fetchErr = flight.Val, flight.Err
	}
	if errors.Is(fetchErr, context.Canceled) {
		return nil, fetchErr
	}
	if fetchErr == nil {
		if shared, ok := result.(*UsageInfo); ok && shared != nil {
			usage := cloneUsageInfo(shared)
			usage.Source = source
			return usage, nil
		}
	}

	degraded := buildLeonardoDegradedUsage(fetchErr)
	degraded.Source = source
	if hasCached {
		cached.Error = degraded.Error
		cached.ErrorCode = degraded.ErrorCode
		cached.NeedsReauth = degraded.NeedsReauth
		cached.Source = source
		return cached, nil
	}
	s.storeLeonardoUsageSnapshot(account.ID, degraded)
	return degraded, nil
}

func (s *AccountUsageService) fetchAndCacheLeonardoUsage(ctx context.Context, account *Account, source string) (*UsageInfo, error) {
	provider := s.leonardoTokenProvider()
	token, err := provider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	sub := account.GetCredential("cognito_sub")
	client := leonardopkg.NewClient(leonardopkg.Options{ProxyURL: accountProxyURL(account)})
	credits, err := client.QueryCredits(ctx, token, sub)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	snapshot, planName := newLeonardoCreditsSnapshot(credits)
	info := &UsageInfo{
		UpdatedAt:            &now,
		LeonardoPlan:         credits.Plan,
		LeonardoPlanName:     planName,
		LeonardoTokenRenewal: credits.TokenRenewalDate,
		LeonardoCredits:      snapshot,
	}
	// 可用积分归零：临时摘出调度，等订阅周期回补（上游不给回补时刻，固定冷却）。
	if credits.UsableTokens <= 0 && s.accountRepo != nil {
		_ = s.accountRepo.SetTempUnschedulable(ctx, account.ID,
			time.Now().Add(leonardoCreditsExhaustedCooldown), "leonardo credits exhausted")
	}
	s.storeLeonardoUsageSnapshot(account.ID, info)
	return info, nil
}

// leonardoTokenProvider 返回账号测试 / 用量拉取共用的 token provider。
// 未注入共享实例时（单测等）退化为仅进程内锁的独立实例。
func (s *AccountUsageService) leonardoTokenProvider() *LeonardoTokenProvider {
	if s.sharedLeonardoTokenProvider != nil {
		return s.sharedLeonardoTokenProvider
	}
	return NewLeonardoTokenProvider(s.accountRepo, nil)
}

// SetLeonardoTokenProvider 注入与网关共享 OAuthRefreshAPI 的 Leonardo token provider。
func (s *AccountUsageService) SetLeonardoTokenProvider(provider *LeonardoTokenProvider) {
	if s != nil {
		s.sharedLeonardoTokenProvider = provider
	}
}

func buildLeonardoDegradedUsage(err error) *UsageInfo {
	now := time.Now()
	info := &UsageInfo{
		UpdatedAt: &now,
		Error:     "failed to fetch Leonardo credits",
		ErrorCode: errorCodeNetworkError,
	}
	if err != nil {
		info.Error = err.Error()
	}
	var authErr *leonardopkg.AuthError
	if errors.As(err, &authErr) {
		info.NeedsReauth = true
	}
	var suspensionErr *leonardopkg.SuspensionError
	if errors.As(err, &suspensionErr) {
		info.Error = "Leonardo account is suspended"
	}
	return info
}
