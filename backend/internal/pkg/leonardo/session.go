package leonardo

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Better Auth 会话 cookie 的关键名字。裸 token 值会被包装成 session_token；
// session_data.* 分片与会话轮转相关，必须随响应合并回写。
const (
	SessionTokenCookieName = "__Secure-better-auth.session_token"
	SessionDataCookiePfx   = "__Secure-better-auth.session_data."
)

// NormalizeCookie 归一化用户粘贴的凭据输入。接受三种形态：
//   - 完整 cookie 串："k1=v1; k2=v2; ..."
//   - 裸 session token 值（自动包装为 __Secure-better-auth.session_token=<值>）
//   - 单个 k=v 对
func NormalizeCookie(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") && strings.Contains(raw, ";") {
		return raw
	}
	if !strings.Contains(raw, "=") {
		return SessionTokenCookieName + "=" + raw
	}
	return raw
}

// ExtractSessionToken 从 cookie 串中提取 __Secure-better-auth.session_token 的值。
func ExtractSessionToken(cookieStr string) string {
	for _, part := range strings.Split(cookieStr, ";") {
		part = strings.TrimSpace(part)
		if value, ok := strings.CutPrefix(part, SessionTokenCookieName+"="); ok {
			return value
		}
	}
	return ""
}

// mergeResponseCookies 把响应 Set-Cookie 合并回本地 cookie 串（Better Auth 会
// 轮换 session_token 与 session_data.* 分片；不回写会导致会话失效）。
// 删除类 cookie（MaxAge<0 或 Expires 已过）会把对应条目移除。
func mergeResponseCookies(existing string, responseCookies []*http.Cookie) string {
	if len(responseCookies) == 0 {
		return existing
	}

	values := make([]string, 0, 16)
	index := make(map[string]int)
	for _, part := range strings.Split(existing, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		name = strings.TrimSpace(name)
		index[name] = len(values)
		values = append(values, part)
	}
	for _, cookie := range responseCookies {
		if cookie == nil || strings.TrimSpace(cookie.Name) == "" {
			continue
		}
		name := strings.TrimSpace(cookie.Name)
		if isCookieDeletion(cookie) {
			if pos, ok := index[name]; ok {
				values = append(values[:pos], values[pos+1:]...)
				delete(index, name)
				for i := pos; i < len(values); i++ {
					cookieName, _, _ := strings.Cut(values[i], "=")
					index[strings.TrimSpace(cookieName)] = i
				}
			}
			continue
		}
		value := name + "=" + cookie.Value
		if pos, ok := index[name]; ok {
			values[pos] = value
		} else {
			index[name] = len(values)
			values = append(values, value)
		}
	}
	return strings.Join(values, "; ")
}

func isCookieDeletion(cookie *http.Cookie) bool {
	if cookie == nil {
		return false
	}
	if cookie.MaxAge < 0 {
		return true
	}
	return !cookie.Expires.IsZero() && cookie.Expires.Before(time.Now())
}

// deletesCriticalSessionCookie 判断响应是否最终删除了关键会话 cookie。
// 同名多条 Set-Cookie 依序生效，只有最后一条的动作决定结果。
func deletesCriticalSessionCookie(responseCookies []*http.Cookie) bool {
	finalDeletion := make(map[string]bool)
	for _, cookie := range responseCookies {
		if cookie == nil {
			continue
		}
		name := strings.TrimSpace(cookie.Name)
		if name == SessionTokenCookieName || strings.HasPrefix(name, SessionDataCookiePfx) {
			finalDeletion[name] = isCookieDeletion(cookie)
		}
	}
	for _, deleted := range finalDeletion {
		if deleted {
			return true
		}
	}
	return false
}

// jwtClaims 是 Cognito id_token 的 payload（仅解码，不验签）。
type jwtClaims struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	Exp           int64    `json:"exp"`
	Iat           int64    `json:"iat"`
	HasuraClaims  string   `json:"https://hasura.io/jwt/claims"`
	CognitoGroups []string `json:"cognito:groups"`
}

// ParseJWT 解码 JWT payload，返回过期时间、Cognito sub 与邮箱。
func ParseJWT(token string) (expiresAt time.Time, sub, email string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, "", "", fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}, "", "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return time.Time{}, "", "", fmt.Errorf("parse JWT claims: %w", err)
	}
	return time.Unix(claims.Exp, 0), claims.Sub, claims.Email, nil
}

// SessionExchange 是一次成功 get-session 交换的结果。
type SessionExchange struct {
	JWT        string
	ExpiresAt  time.Time
	CognitoSub string
	Email      string
	// RotatedCookie 是合并了响应 Set-Cookie 之后的完整 cookie 串；
	// CookieRotated 为 false 时与输入一致（无需写回）。
	RotatedCookie string
	CookieRotated bool
}

// extractJWT 按优先级探测响应里的 JWT（session.token → session.idToken →
// session.accessToken → token → idToken → user.token，须含 "."）。
func extractJWT(data map[string]any) string {
	pick := func(token any) string {
		if s, ok := token.(string); ok && strings.Contains(s, ".") {
			return s
		}
		return ""
	}
	if sess, ok := data["session"].(map[string]any); ok {
		for _, key := range []string{"token", "idToken", "accessToken"} {
			if jwt := pick(sess[key]); jwt != "" {
				return jwt
			}
		}
	}
	for _, key := range []string{"token", "idToken"} {
		if jwt := pick(data[key]); jwt != "" {
			return jwt
		}
	}
	if user, ok := data["user"].(map[string]any); ok {
		if jwt := pick(user["token"]); jwt != "" {
			return jwt
		}
	}
	return ""
}
