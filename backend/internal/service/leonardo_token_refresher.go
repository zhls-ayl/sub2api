package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	leonardo "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
)

// leonardoRefreshWindow 是 JWT 剩余寿命低于该值时触发刷新的阈值。
// Leonardo 的 Cognito JWT 约 1 小时寿命。窗口必须大于出图提交后的脱钩时限
// （leonardoImageDetachedTimeout = 10 分钟）：交给出图的 JWT 要撑完整个轮询，
// 否则轮询中途 401 只能放弃一张已扣积分的图。
const leonardoRefreshWindow = 15 * time.Minute

// leonardoTokenCredentialKeys 是一次 cookie→JWT 交换实际产出的字段；持久化时
// 只写这些。cookie 本身可能被 Better Auth 轮转（轮转后的新 cookie 也属于交换
// 产物，必须一并写回，否则会话失效），model_mapping 等管理员维护的字段以 DB
// 当前值为准。
var leonardoTokenCredentialKeys = []string{"access_token", "expires_at", "cognito_sub", "cookie"}

// LeonardoTokenCredentialsRepository 是 Leonardo 刷新结果的条件写入边界。
//
// 实现必须：仅当账号当前 cookie 仍等于 expectedCookie（交换所用的那份）时，
// 把 tokenFields 合并进 credentials（不整体覆盖）。applied=false 表示 cookie
// 已被管理员替换或账号不存在。
type LeonardoTokenCredentialsRepository interface {
	UpdateLeonardoTokenIfCookieUnchanged(
		ctx context.Context,
		id int64,
		expectedCookie string,
		tokenFields map[string]any,
	) (applied bool, err error)
}

// LeonardoTokenInvalidationRepository 是上游拒绝 JWT 时的条件清除边界。
//
// 实现必须：仅当账号当前 access_token 仍等于 expectedAccessToken 时，删掉
// access_token / expires_at（cookie 等字段保留）——并发请求可能已刷新出新
// token，不能误删。
type LeonardoTokenInvalidationRepository interface {
	InvalidateLeonardoAccessTokenIfUnchanged(
		ctx context.Context,
		id int64,
		expectedAccessToken string,
	) (applied bool, err error)
}

// LeonardoConditionalErrorRepository 是 cookie 失效时的条件置错边界。
//
// 实现必须仅当账号当前 cookie 仍等于 expectedCookie 时才置 error：上游拒绝
// 的是那份 cookie，管理员往返期间换上的新 cookie 不能被旧结论隔离。
type LeonardoConditionalErrorRepository interface {
	SetLeonardoErrorIfCookieUnchanged(
		ctx context.Context,
		id int64,
		expectedCookie string,
		errorMsg string,
	) (applied bool, err error)
}

// setLeonardoCookieRefreshError 把 cookie 失效落到账号 error。仓储支持条件
// 写入时按 cookie 做 CAS；否则退回无条件 SetError。
func setLeonardoCookieRefreshError(ctx context.Context, repo AccountRepository, account *Account, errorMsg string) (applied bool, err error) {
	if conditional, ok := repo.(LeonardoConditionalErrorRepository); ok {
		if cookie := account.GetCredential("cookie"); strings.TrimSpace(cookie) != "" {
			return conditional.SetLeonardoErrorIfCookieUnchanged(ctx, account.ID, cookie, errorMsg)
		}
	}
	if err := repo.SetError(ctx, account.ID, errorMsg); err != nil {
		return false, err
	}
	return true, nil
}

// leonardoTokenCredentialFields 从刷新后的完整 credentials 中挑出交换产物字段。
func leonardoTokenCredentialFields(credentials map[string]any) map[string]any {
	fields := make(map[string]any, len(leonardoTokenCredentialKeys))
	for _, key := range leonardoTokenCredentialKeys {
		if value, ok := credentials[key]; ok {
			fields[key] = value
		}
	}
	return fields
}

// leonardoTokenExpiry 解析 credentials 里 RFC3339 的 expires_at；缺失或无法
// 解析返回零值（视为需要刷新，交换很廉价）。
func leonardoTokenExpiry(credentials map[string]any) (time.Time, bool) {
	raw, _ := credentials["expires_at"].(string)
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return expiresAt, true
}

// leonardoTokenExpired 判断 JWT 是否缺失或距过期不足 window。
func leonardoTokenExpired(credentials map[string]any, window time.Duration) bool {
	token, _ := credentials["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return true
	}
	expiresAt, ok := leonardoTokenExpiry(credentials)
	if !ok {
		return true
	}
	return time.Until(expiresAt) < window
}

// LeonardoTokenRefresher 用账号里的长期 Better Auth cookie 换取短寿 JWT。
//
// 与多数平台的区别：Leonardo 没有 refresh_token，长期凭据是浏览器 cookie，
// 且 get-session 会轮转 cookie——刷新产物包含新的 cookie 串，必须随 token
// 一起 CAS 写回。cookie 彻底失效只能由用户重新导出，此类失败必须让账号进
// error 态而不是无限重试。
type LeonardoTokenRefresher struct {
	clients *leonardoClientCache
}

// NewLeonardoTokenRefresher 构造 refresher。
func NewLeonardoTokenRefresher() *LeonardoTokenRefresher {
	return &LeonardoTokenRefresher{clients: &leonardoClientCache{}}
}

func (r *LeonardoTokenRefresher) CacheKey(account *Account) string {
	if account == nil {
		return ""
	}
	return "leonardo:token:" + strconv.FormatInt(account.ID, 10)
}

func (r *LeonardoTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformLeonardo &&
		account.Type == AccountTypeOAuth &&
		strings.TrimSpace(account.GetCredential("cookie")) != ""
}

// NeedsRefresh 在 JWT 缺失或即将过期时返回 true。refreshWindow 由调度方传入，
// 但取传入值与 leonardoRefreshWindow 中较大的那个，避免窗口过小刷不及。
func (r *LeonardoTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	window := refreshWindow
	if window < leonardoRefreshWindow {
		window = leonardoRefreshWindow
	}
	return leonardoTokenExpired(account.Credentials, window)
}

// Refresh 用 cookie 换新 JWT，返回保留了原有全部字段的 credentials。
// Better Auth 的 cookie 轮转（含分片 session_data.*）会体现在新 cookie 串里。
func (r *LeonardoTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if !r.CanRefresh(account) {
		return nil, errors.New("account is not a refreshable leonardo account")
	}
	client := r.clients.clientForAccount(account)
	exchange, err := client.RefreshSession(ctx, account.GetCredential("cookie"))
	if err != nil {
		var authErr *leonardo.AuthError
		if errors.As(err, &authErr) {
			// cookie 已彻底失效，重试没有意义——包装成明确信息让上层置 error。
			return nil, fmt.Errorf("leonardo cookie is no longer valid, re-export it from the browser: %w", err)
		}
		return nil, fmt.Errorf("refresh leonardo token: %w", err)
	}
	if exchange.JWT == "" {
		return nil, errors.New("refresh leonardo token: no JWT in session exchange")
	}
	return MergeCredentials(account.Credentials, map[string]any{
		"access_token": exchange.JWT,
		"expires_at":   exchange.ExpiresAt.UTC().Format(time.RFC3339),
		"cognito_sub":  exchange.CognitoSub,
		// 写回轮转后的完整 cookie（未轮转时是归一化形态，同样安全）。
		"cookie": exchange.RotatedCookie,
	}), nil
}

// leonardoClientCache 按账号缓存带代理的客户端实例（HTTP 连接复用；客户端
// 无状态、无 cookie jar，按代理指纹缓存即可）。代理变化时重建。
type leonardoClientCache struct {
	mu      sync.Mutex
	clients map[int64]*leonardoClientEntry
	// options 是构造客户端的模板（单测注入端点用）；ProxyURL 总是取账号自己的。
	options leonardo.Options
}

type leonardoClientEntry struct {
	client   *leonardo.Client
	proxyURL string
}

func (c *leonardoClientCache) clientForAccount(account *Account) *leonardo.Client {
	proxyURL := accountProxyURL(account)
	if account == nil || account.ID <= 0 {
		return c.newClient(proxyURL)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients == nil {
		c.clients = make(map[int64]*leonardoClientEntry)
	}
	if entry, ok := c.clients[account.ID]; ok && entry.proxyURL == proxyURL {
		return entry.client
	}
	client := c.newClient(proxyURL)
	c.clients[account.ID] = &leonardoClientEntry{client: client, proxyURL: proxyURL}
	return client
}

func (c *leonardoClientCache) newClient(proxyURL string) *leonardo.Client {
	opts := c.options
	opts.ProxyURL = proxyURL
	return leonardo.NewClient(opts)
}

// markLeonardoCookieRefreshPermanentFailure 把 cookie 换 JWT 的终态失败落到
// 账号 error。SetError 失败只记日志：调用方仍必须看到原来的刷新错误。
func markLeonardoCookieRefreshPermanentFailure(ctx context.Context, repo AccountRepository, account *Account, err error) {
	if repo == nil || account == nil || account.ID <= 0 || !isNonRetryableRefreshError(err) {
		return
	}
	errorMsg := "Token refresh failed (non-retryable): " + err.Error()
	applied, setErr := setLeonardoCookieRefreshError(ctx, repo, account, errorMsg)
	if setErr != nil {
		slog.Warn("failed to persist leonardo cookie refresh error status",
			"account_id", account.ID, "error", setErr)
		return
	}
	if !applied {
		slog.Info("leonardo_cookie_refresh_error_skipped_cookie_changed", "account_id", account.ID)
	}
}
