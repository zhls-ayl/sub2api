package leonardo

import (
	"strings"
)

// 错误来源分类。
const (
	ErrorTypeStatus     = "status"     // 上游返回了非 2xx 状态码
	ErrorTypeTimeout    = "timeout"    // 请求超时
	ErrorTypeConnection = "connection" // 连接失败
	ErrorTypeProxy      = "proxy"      // 代理故障
	ErrorTypeNetwork    = "network"    // 其它网络层错误
)

// RequestError 是 Leonardo 直连的基础错误类型，同时也是「终态错误」的语义：
// 请求本身有问题（参数校验失败、模型不支持等），换账号重试无用。
//
// AuthError / RateLimitedError / SuspensionError / InsufficientTokensError /
// ContentRejectedError / UpstreamTemporaryError 内嵌本类型并实现 Unwrap，
// 因此 errors.As(err, &*RequestError) 对所有子类成立；区分类别用具体类型。
type RequestError struct {
	Message string
	// StatusCode 是上游 HTTP 状态码；网络层错误时为 0。
	StatusCode int
	// ErrorType 是来源分类，取上面的 ErrorType* 常量。
	ErrorType string
	// UserMessage 是可直接展示给终端用户的文案；为空时回落到 Message。
	UserMessage string
}

func (e *RequestError) Error() string { return e.Message }

// User 返回面向用户的文案。
func (e *RequestError) User() string {
	if msg := strings.TrimSpace(e.UserMessage); msg != "" {
		return strings.TrimSpace(msg)
	}
	return strings.TrimSpace(e.Message)
}

// IsNetwork 报告错误是否为网络层（连接 / 超时 / 代理）错误。提交阶段出现
// 这类错误时无法判断上游是否已受理并扣除积分（indeterminate），调用方
// 不得自动重提。
func (e *RequestError) IsNetwork() bool {
	switch e.ErrorType {
	case ErrorTypeTimeout, ErrorTypeConnection, ErrorTypeProxy, ErrorTypeNetwork:
		return true
	default:
		return false
	}
}

// AuthError 表示凭据失效（get-session 401/403，或 GraphQL unauthorized/JWT 错误）。
type AuthError struct{ RequestError }

func (e *AuthError) Unwrap() error { return &e.RequestError }

// RateLimitedError 表示上游限流（429）。账号应冷却，稍后可继续使用。
type RateLimitedError struct{ RequestError }

func (e *RateLimitedError) Unwrap() error { return &e.RequestError }

// SuspensionError 表示账号被封禁（上游 "suspended for violating our user
// agreement" 错误串）。账号应置 error，不再参与调度。
type SuspensionError struct{ RequestError }

func (e *SuspensionError) Unwrap() error { return &e.RequestError }

// InsufficientTokensError 表示账号积分不足（GraphQL "insufficient tokens"）。
// 凭据仍有效，换号即可；余额快照应刷新。
type InsufficientTokensError struct{ RequestError }

func (e *InsufficientTokensError) Unwrap() error { return &e.RequestError }

// ContentRejectedError 表示内容安全拒绝（init image moderation REJECTED/
// BLOCKED，或生成失败的 PROVIDER_MODERATION_ERROR / NSFW 分类）。同样的
// prompt 打原生号大概率再次被拒，应跳过剩余原生号。
type ContentRejectedError struct {
	RequestError
	// ProviderModeration 为 true 表示是上游模型方（如 OpenAI）的审核拦截
	// （PROVIDER_MODERATION_ERROR）：结果不稳定，同一提示词重提可能通过，
	// 调用方可重试一次。Leonardo 自身拦截与参考图审核为 false。
	ProviderModeration bool
}

func (e *ContentRejectedError) Unwrap() error { return &e.RequestError }

// UpstreamTemporaryError 表示上游临时故障（5xx / 生成失败的 PROVIDER_RATE_
// LIMIT / PROVIDER_INTERNAL_ERROR / PROVIDER_TIMEOUT / ALL_PROVIDERS_FAILED 等），
// 可换号重试。
type UpstreamTemporaryError struct{ RequestError }

func (e *UpstreamTemporaryError) Unwrap() error { return &e.RequestError }

// NewRequestError 构造终态错误。
func NewRequestError(message string) *RequestError {
	return &RequestError{Message: message}
}

// NewAuthError 构造鉴权错误。
func NewAuthError(message string, statusCode int) *AuthError {
	return &AuthError{RequestError{Message: message, StatusCode: statusCode, ErrorType: ErrorTypeStatus}}
}

// NewRateLimitedError 构造限流错误。
func NewRateLimitedError(message string, statusCode int) *RateLimitedError {
	return &RateLimitedError{RequestError{Message: message, StatusCode: statusCode, ErrorType: ErrorTypeStatus}}
}

// NewSuspensionError 构造封号错误。
func NewSuspensionError(message string) *SuspensionError {
	return &SuspensionError{RequestError{Message: message, ErrorType: ErrorTypeStatus}}
}

// NewInsufficientTokensError 构造积分不足错误。
func NewInsufficientTokensError(message string) *InsufficientTokensError {
	return &InsufficientTokensError{RequestError{Message: message, ErrorType: ErrorTypeStatus}}
}

// NewContentRejectedError 构造内容拒绝错误。
func NewContentRejectedError(message string) *ContentRejectedError {
	return &ContentRejectedError{RequestError: RequestError{Message: message, ErrorType: ErrorTypeStatus}}
}

// NewUpstreamTemporaryError 构造上游临时错误。
func NewUpstreamTemporaryError(message string, statusCode int) *UpstreamTemporaryError {
	return &UpstreamTemporaryError{RequestError{Message: message, StatusCode: statusCode, ErrorType: ErrorTypeStatus}}
}

// 上游判定串（大小写不敏感匹配）。
const (
	suspensionMarker     = "suspended for violating our user agreement"
	insufficientTokensA  = "insufficient tokens"
	insufficientTokensB  = "insufficient_tokens"
	unauthorizedMarkerA  = "unauthorized"
	unauthorizedMarkerB  = "jwt"
	htmlChallengeSnippet = "<!doctype html"
)

// classifyGraphQLErrorMessage 按 GraphQL errors[].message 文本归类。
func classifyGraphQLErrorMessage(message string) error {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(lower, suspensionMarker):
		return &SuspensionError{RequestError{Message: message, ErrorType: ErrorTypeStatus}}
	case strings.Contains(lower, insufficientTokensA) || strings.Contains(lower, insufficientTokensB):
		return &InsufficientTokensError{RequestError{Message: message, ErrorType: ErrorTypeStatus}}
	case strings.Contains(lower, unauthorizedMarkerA) || strings.Contains(lower, unauthorizedMarkerB):
		return NewAuthError(message, 0)
	default:
		return &RequestError{Message: message}
	}
}
