package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
)

const (
	// leonardoTokenUsableSkew 是旧 JWT 在刷新失败时仍可继续使用的最小剩余寿命。
	leonardoTokenUsableSkew = time.Minute
	// leonardoLockWaitTime 是其它实例持有刷新锁、且本地 JWT 已不可用时的等待时长。
	leonardoLockWaitTime = 200 * time.Millisecond
)

// LeonardoTokenProvider 为 Leonardo OAuth（cookie）账号提供短寿 JWT。
//
// 后台 TokenRefreshService 负责保温，但请求仍可能落到空 token 或临期 token
// （新账号、错过一轮）。请求路径走与后台相同的 OAuthRefreshAPI：进程内锁 +
// Redis 锁 + DB 重读 + 二次检查，并发请求只打一次 get-session，刷新结果按
// cookie 条件 CAS 落库（含 Better Auth cookie 轮转写回）。
type LeonardoTokenProvider struct {
	accountRepo AccountRepository
	refreshAPI  *OAuthRefreshAPI
	refresher   *LeonardoTokenRefresher
}

// NewLeonardoTokenProvider 构造 provider。refreshAPI 为 nil 时退化为仅进程内
// 锁的独立实例。
func NewLeonardoTokenProvider(accountRepo AccountRepository, refreshAPI *OAuthRefreshAPI) *LeonardoTokenProvider {
	if refreshAPI == nil {
		refreshAPI = NewOAuthRefreshAPI(accountRepo, nil)
	}
	return &LeonardoTokenProvider{
		accountRepo: accountRepo,
		refreshAPI:  refreshAPI,
		refresher:   NewLeonardoTokenRefresher(),
	}
}

// GetAccessToken 返回可用的 Leonardo JWT。
//
// 刷新失败时的取舍：
//   - cookie 已失效（AuthError）：账号置 error 并返回错误，即便旧 JWT 未过期；
//   - 账号在锁内重读时已被停用或删掉 cookie：尊重管理员操作，返回错误；
//   - 其它失败（get-session 5xx/429/超时、DB 落库失败、锁竞争）：旧 JWT 仍可用
//     就继续用，交给上游 401 做权威裁决。
//
// 调用方传入的 account 是调度快照，这里不修改其 Credentials。
func (p *LeonardoTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	token := strings.TrimSpace(account.GetCredential("access_token"))
	if p == nil || p.refresher == nil || p.refreshAPI == nil {
		if token == "" {
			return "", errors.New("access_token not found in credentials")
		}
		return token, nil
	}

	if token != "" && !leonardoTokenExpired(account.Credentials, leonardoRefreshWindow) {
		return token, nil
	}
	if !p.refresher.CanRefresh(account) {
		if token == "" {
			return "", errors.New("access_token not found in credentials")
		}
		return token, nil
	}

	usable := !leonardoTokenExpired(account.Credentials, leonardoTokenUsableSkew)
	result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), account, p.refresher, leonardoRefreshWindow)
	if err != nil {
		var authErr *leonardopkg.AuthError
		switch {
		case errors.As(err, &authErr):
			markLeonardoCookieRefreshPermanentFailure(ctx, p.accountRepo, account, err)
			return "", err
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.Is(err, errOAuthRefreshAccountStateChanged):
			return "", err
		case usable:
			slog.Warn("leonardo_token_refresh_failed_use_existing",
				"account_id", account.ID, "error", err)
			return token, nil
		default:
			return "", err
		}
	}

	if result.LockHeld {
		if usable {
			return token, nil
		}
		return p.waitForConcurrentRefresh(ctx, account)
	}

	if result.Account != nil {
		if refreshed := strings.TrimSpace(result.Account.GetCredential("access_token")); refreshed != "" {
			return refreshed, nil
		}
	}
	if usable {
		return token, nil
	}
	return "", errors.New("leonardo refresh returned no usable access_token")
}

// waitForConcurrentRefresh 在其它实例持有刷新锁时短暂等待，再从 DB 读一次结果。
func (p *LeonardoTokenProvider) waitForConcurrentRefresh(ctx context.Context, account *Account) (string, error) {
	timer := time.NewTimer(leonardoLockWaitTime)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
	}
	if p.accountRepo != nil {
		latest, err := p.accountRepo.GetByID(ctx, account.ID)
		if err == nil && latest != nil {
			if refreshed := strings.TrimSpace(latest.GetCredential("access_token")); refreshed != "" {
				return refreshed, nil
			}
		}
	}
	return "", errors.New("leonardo token refresh in progress")
}

func (s *GatewayService) getLeonardoOAuthToken(ctx context.Context, account *Account) (string, error) {
	return s.leonardoTokenProvider.GetAccessToken(ctx, account)
}
