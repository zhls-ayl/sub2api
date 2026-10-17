package leonardo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// SessionURL 用浏览器 cookie 换取短寿 JWT（Better Auth get-session）。
	SessionURL = "https://app.leonardo.ai/api/auth/get-session"
	// GraphQLURL 是 Leonardo 网页版的全部业务操作端点。
	GraphQLURL = "https://api.leonardo.ai/v1/graphql"

	defaultSchemaVersion = "latest"
	chromeUserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	maxErrorBodySnippet  = 500
)

// Options 配置 Client。
type Options struct {
	// SchemaVersion 覆盖 X-Leo-Schema-Version 头（默认 latest；上游强制钉
	// 版本时可配置为具体版本号，如 1.274.2）。
	SchemaVersion string
	// ProxyURL 可选的出站代理。
	ProxyURL string
	// HTTPClient 可选的自定义 http.Client（测试注入用）。
	HTTPClient *http.Client
	// SessionURL / GraphQLURL 覆盖默认端点（测试注入用）。
	SessionURL string
	GraphQLURL string
}

// Client 是 Leonardo 上游的无状态客户端：不持有账号会话，每次调用显式
// 传入 JWT；cookie→JWT 的交换由 RefreshSession 完成，凭据生命周期归
// service 层管理。
type Client struct {
	httpClient    *http.Client
	schemaVersion string
	sessionURL    string
	graphqlURL    string
}

// NewClient 构造客户端。
func NewClient(opts Options) *Client {
	schemaVersion := strings.TrimSpace(opts.SchemaVersion)
	if schemaVersion == "" {
		schemaVersion = defaultSchemaVersion
	}
	transport := http.DefaultTransport
	if opts.ProxyURL != "" {
		if parsed, err := url.Parse(opts.ProxyURL); err == nil {
			proxyTransport := http.DefaultTransport.(*http.Transport).Clone()
			proxyTransport.Proxy = http.ProxyURL(parsed)
			transport = proxyTransport
		}
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Transport: transport, Timeout: 120 * time.Second}
	}
	sessionURL := strings.TrimSpace(opts.SessionURL)
	if sessionURL == "" {
		sessionURL = SessionURL
	}
	graphqlURL := strings.TrimSpace(opts.GraphQLURL)
	if graphqlURL == "" {
		graphqlURL = GraphQLURL
	}
	return &Client{httpClient: client, schemaVersion: schemaVersion, sessionURL: sessionURL, graphqlURL: graphqlURL}
}

// graphqlRequest 是 GraphQL 调用请求体。
type graphqlRequest struct {
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
	Query         string         `json:"query"`
}

// RefreshSession 用 cookie 调 get-session 换取新 JWT。
//
// Better Auth 可能轮换会话 cookie：非破坏性轮转立即体现在返回的
// RotatedCookie 中（调用方必须 CAS 写回存储）；删除关键会话 cookie 的动作
// 只有在新 JWT 验证有效后才随 RotatedCookie 生效——否则上游登出响应会把
// 可恢复的 cookie 永久替换成无关 cookie。
func (c *Client) RefreshSession(ctx context.Context, rawCookie string) (*SessionExchange, error) {
	cookieStr := NormalizeCookie(rawCookie)
	if cookieStr == "" {
		return nil, NewRequestError("leonardo cookie is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sessionURL, nil)
	if err != nil {
		return nil, &RequestError{Message: fmt.Sprintf("create get-session request: %v", err), ErrorType: ErrorTypeNetwork}
	}
	req.Close = true
	req.Header.Set("Cookie", cookieStr)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", chromeUserAgent)
	req.Header.Set("Referer", "https://app.leonardo.ai/")
	req.Header.Set("Origin", "https://app.leonardo.ai")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, wrapNetworkError("get-session request failed", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, wrapNetworkError("read get-session response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifySessionHTTPError(resp.StatusCode, body)
	}

	responseCookies := resp.Cookies()
	mergedCookie := mergeResponseCookies(cookieStr, responseCookies)
	// 删除关键会话 cookie 的动作只有在拿到有效 JWT 后才允许生效；否则上游
	// 登出响应会把可恢复的 cookie 永久替换成无关 cookie。
	deferCriticalDeletion := mergedCookie != cookieStr && deletesCriticalSessionCookie(responseCookies)

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse session response: %v", err))
	}
	jwt := extractJWT(result)
	if jwt == "" {
		// 无 JWT：非破坏性轮转仍需写回（下次刷新可凭新 cookie 恢复），
		// 随错误一并返回；破坏性删除则丢弃。
		if mergedCookie != cookieStr && !deferCriticalDeletion {
			return &SessionExchange{RotatedCookie: mergedCookie, CookieRotated: true}, NewRequestError("no JWT found in session response")
		}
		return nil, NewRequestError("no JWT found in session response")
	}
	expiresAt, sub, email, err := ParseJWT(jwt)
	if err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse JWT: %v", err))
	}
	return &SessionExchange{
		JWT:           jwt,
		ExpiresAt:     expiresAt,
		CognitoSub:    sub,
		Email:         email,
		RotatedCookie: mergedCookie,
		CookieRotated: mergedCookie != cookieStr,
	}, nil
}

func classifySessionHTTPError(statusCode int, body []byte) error {
	if statusCode == http.StatusTooManyRequests {
		return NewRateLimitedError("leonardo rate limited get-session (429)", statusCode)
	}
	snippet := strings.TrimSpace(string(body))
	lower := strings.ToLower(snippet)
	if strings.HasPrefix(lower, htmlChallengeSnippet) || strings.HasPrefix(lower, "<html") {
		// Cloudflare / Vercel 挑战页：瞬时，不代表 cookie 失效。
		return NewUpstreamTemporaryError(fmt.Sprintf("get-session returned %d with an HTML challenge page", statusCode), statusCode)
	}
	if snippet == "" {
		return NewUpstreamTemporaryError(fmt.Sprintf("get-session returned %d", statusCode), statusCode)
	}
	if len(snippet) > maxErrorBodySnippet {
		snippet = snippet[:maxErrorBodySnippet]
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return NewAuthError(fmt.Sprintf("get-session returned %d: %s", statusCode, snippet), statusCode)
	}
	return NewUpstreamTemporaryError(fmt.Sprintf("get-session returned %d: %s", statusCode, snippet), statusCode)
}

// doGraphQL 执行一次 GraphQL 调用并返回原始响应体。
func (c *Client) doGraphQL(ctx context.Context, jwt, operationName string, variables map[string]any, query string) ([]byte, error) {
	reqBody, err := json.Marshal(graphqlRequest{OperationName: operationName, Variables: variables, Query: query})
	if err != nil {
		return nil, NewRequestError(fmt.Sprintf("marshal %s request: %v", operationName, err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, &RequestError{Message: fmt.Sprintf("create %s request: %v", operationName, err), ErrorType: ErrorTypeNetwork}
	}
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://app.leonardo.ai")
	req.Header.Set("Referer", "https://app.leonardo.ai/")
	req.Header.Set("User-Agent", chromeUserAgent)
	req.Header.Set("X-Leo-Schema-Version", c.schemaVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, wrapNetworkError(fmt.Sprintf("%s request failed", operationName), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, wrapNetworkError(fmt.Sprintf("read %s response", operationName), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyGraphQLHTTPError(operationName, resp.StatusCode, body)
	}
	return body, nil
}

func classifyGraphQLHTTPError(operationName string, statusCode int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > maxErrorBodySnippet {
		snippet = snippet[:maxErrorBodySnippet]
	}
	message := fmt.Sprintf("%s graphql returned %d", operationName, statusCode)
	if snippet != "" {
		message += ": " + snippet
	}
	switch {
	case statusCode == http.StatusTooManyRequests:
		return NewRateLimitedError(message, statusCode)
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return NewAuthError(message, statusCode)
	case statusCode >= 500:
		return NewUpstreamTemporaryError(message, statusCode)
	default:
		return NewRequestError(message)
	}
}

// firstGraphQLError 解析 GraphQL errors[]；message 命中判定串时返回归类错误。
func firstGraphQLError(operationName string, body []byte) error {
	var gqlResp struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Details json.RawMessage `json:"details"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil
	}
	for _, gqlErr := range gqlResp.Errors {
		msg := graphQLErrorMessage(gqlErr.Message, gqlErr.Extensions.Details)
		if msg == "" {
			continue
		}
		if classified := classifyGraphQLErrorMessage(msg); classified != nil {
			return classified
		}
		return NewRequestError(fmt.Sprintf("%s graphql error: %s", operationName, msg))
	}
	return nil
}

// graphQLErrorMessage 拼出一条 GraphQL 错误的完整信息。Leonardo 的参数校验
// 错误顶层 message 只有 "An error occurred."，真正原因（如 width / height 的
// 允许值清单）在 extensions.details.{code,message} 里，这里把它接在顶层
// message 后面，日志与回给客户端的错误都能看到；顶层 message 仍保留在前，
// 不影响 classifyGraphQLErrorMessage 的判定串匹配。
func graphQLErrorMessage(message string, rawDetails json.RawMessage) string {
	message = strings.TrimSpace(message)
	var details struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(rawDetails) == 0 || json.Unmarshal(rawDetails, &details) != nil {
		return message
	}
	detail := strings.TrimSpace(details.Message)
	if detail == "" || detail == message {
		return message
	}
	if code := strings.TrimSpace(details.Code); code != "" {
		detail = code + ": " + detail
	}
	if message == "" {
		return detail
	}
	return message + " (" + detail + ")"
}

func wrapNetworkError(prefix string, err error) error {
	message := fmt.Sprintf("%s: %v", prefix, err)
	errType := ErrorTypeNetwork
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "Client.Timeout"), strings.Contains(msg, "timeout"):
		errType = ErrorTypeTimeout
	case strings.Contains(msg, "proxyconnect"):
		errType = ErrorTypeProxy
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "dial tcp"), strings.Contains(msg, "no such host"):
		errType = ErrorTypeConnection
	}
	return &RequestError{Message: message, ErrorType: errType}
}

// DownloadImage 下载产物字节。Leonardo CDN URL 有过期时间，完成后应立即下载。
// 大小上限 32MB（与 ImageResultUploader 的转存上限一致）。
func (c *Client) DownloadImage(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &RequestError{Message: fmt.Sprintf("create image download request: %v", err), ErrorType: ErrorTypeNetwork}
	}
	req.Header.Set("Accept", "image/*")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, wrapNetworkError("image download failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, NewUpstreamTemporaryError(fmt.Sprintf("image download returned %d", resp.StatusCode), resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if err != nil {
		return nil, wrapNetworkError("read image download response", err)
	}
	if len(data) > 32<<20 {
		return nil, NewRequestError("generated image exceeds 32MB")
	}
	return data, nil
}

// Credits 是账号积分快照。
type Credits struct {
	Plan               string
	PaidTokens         int
	SubscriptionTokens int
	RolloverTokens     int
	// TotalTokens 是三池之和（展示用）。
	TotalTokens int
	// UsableTokens 是出图真正可用的积分：按套餐能力剔除不可用的池（免费套餐的
	// Rollover、无订阅的 Top-up）。未知套餐不推断，等于 TotalTokens。
	UsableTokens     int
	TokenRenewalDate string
}

// usableTokens 按套餐能力计算出图可用积分。
func usableTokens(plan string, subscription, rollover, paid int) int {
	quota, ok := LookupPlan(plan)
	if !ok {
		return subscription + rollover + paid
	}
	usable := subscription
	if quota.CanUseRollover {
		usable += rollover
	}
	if quota.CanUseTopUp {
		usable += paid
	}
	return usable
}

const userTokensQuery = `query GetUserTokensFromSub($sub: String) {
  user_details(where: {cognitoId: {_eq: $sub}}) {
    id
    plan
    tokenRenewalDate
    paidTokens
    subscriptionTokens
    rolloverTokens
  }
}`

// QueryCredits 按 JWT sub 查询账号积分（subscription + paid + rollover）。
func (c *Client) QueryCredits(ctx context.Context, jwt, sub string) (*Credits, error) {
	body, err := c.doGraphQL(ctx, jwt, "GetUserTokensFromSub", map[string]any{"sub": sub}, userTokensQuery)
	if err != nil {
		return nil, err
	}
	if gqlErr := firstGraphQLError("GetUserTokensFromSub", body); gqlErr != nil {
		return nil, gqlErr
	}
	var gqlResp struct {
		Data struct {
			UserDetails []struct {
				Plan               string `json:"plan"`
				PaidTokens         int    `json:"paidTokens"`
				SubscriptionTokens int    `json:"subscriptionTokens"`
				RolloverTokens     int    `json:"rolloverTokens"`
				TokenRenewalDate   string `json:"tokenRenewalDate"`
			} `json:"user_details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse GetUserTokensFromSub response: %v", err))
	}
	if len(gqlResp.Data.UserDetails) == 0 {
		return nil, NewRequestError("no user details found for sub")
	}
	ud := gqlResp.Data.UserDetails[0]
	return &Credits{
		Plan:               ud.Plan,
		PaidTokens:         ud.PaidTokens,
		SubscriptionTokens: ud.SubscriptionTokens,
		RolloverTokens:     ud.RolloverTokens,
		TotalTokens:        ud.PaidTokens + ud.SubscriptionTokens + ud.RolloverTokens,
		UsableTokens:       usableTokens(ud.Plan, ud.SubscriptionTokens, ud.RolloverTokens, ud.PaidTokens),
		TokenRenewalDate:   ud.TokenRenewalDate,
	}, nil
}
