package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

// NewOpenAIOAuthClient creates a new OpenAI OAuth client
func NewOpenAIOAuthClient() service.OpenAIOAuthClient {
	return &openaiOAuthService{tokenURL: openai.TokenURL}
}

type openaiOAuthService struct {
	tokenURL string
}

// tokenURLForClient follows the official Excel client's token contract, which
// targets the unified token endpoint. Other query parameters are kept.
func (s *openaiOAuthService) tokenURLForClient(clientID string) string {
	if clientID != openai.ExcelClientID {
		return s.tokenURL
	}
	u, err := url.Parse(s.tokenURL)
	if err != nil {
		return s.tokenURL
	}
	query := u.Query()
	query.Set("unified", "true")
	u.RawQuery = query.Encode()
	return u.String()
}

// excelOAuthFailureCodes are the OAuth error codes an Excel token failure may
// name. The token refresh service classifies failures by these codes in the
// error text (non-retryable credentials, refresh races), so they survive while
// the rest of the response body, which can echo credentials, is dropped.
var excelOAuthFailureCodes = map[string]struct{}{
	"invalid_grant": {}, "invalid_refresh_token": {}, "token_expired": {},
	"app_session_terminated": {}, "refresh_token_reused": {}, "refresh_token_invalidated": {},
	"invalid_client": {}, "unauthorized_client": {}, "access_denied": {},
	"invalid_scope": {}, "invalid_request": {},
}

// excelOAuthFailure formats an Excel token endpoint rejection with the status
// and, when allowlisted, the OAuth error code only.
func excelOAuthFailure(code, action string, resp *req.Response) error {
	message := fmt.Sprintf("Excel OAuth token %s failed: status %d", action, resp.StatusCode)
	body := resp.Bytes()
	errorCode := gjson.GetBytes(body, "error").String()
	if gjson.GetBytes(body, "error").IsObject() {
		errorCode = gjson.GetBytes(body, "error.code").String()
	}
	if _, ok := excelOAuthFailureCodes[errorCode]; ok {
		message += ", error: " + errorCode
	}
	return infraerrors.New(http.StatusBadGateway, code, message)
}

func (s *openaiOAuthService) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}

	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("code", code)
	formData.Set("redirect_uri", redirectURI)
	formData.Set("code_verifier", codeVerifier)

	var tokenResp openai.TokenResponse

	request := client.R().
		SetContext(ctx).
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp)
	// The Excel client does not identify as Codex.
	if clientID != openai.ExcelClientID {
		authUA, authOriginator := service.CodexCanonicalAuthIdentity()
		request.SetHeader("User-Agent", authUA).SetHeader("originator", authOriginator)
	}
	resp, err := request.Post(s.tokenURLForClient(clientID))

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		if clientID == openai.ExcelClientID {
			return nil, excelOAuthFailure("OPENAI_OAUTH_TOKEN_EXCHANGE_FAILED", "exchange", resp)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_TOKEN_EXCHANGE_FAILED", "token exchange failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	return &tokenResp, nil
}

func (s *openaiOAuthService) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, "")
}

func (s *openaiOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	// 调用方应始终传入正确的 client_id；为兼容旧数据，未指定时默认使用 OpenAI ClientID
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	return s.refreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

func (s *openaiOAuthService) refreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	formData := url.Values{}
	formData.Set("grant_type", "refresh_token")
	formData.Set("refresh_token", refreshToken)
	formData.Set("client_id", clientID)
	// The Excel client refreshes without a scope parameter.
	if clientID != openai.ExcelClientID {
		formData.Set("scope", openai.RefreshScopes)
	}

	var tokenResp openai.TokenResponse

	request := client.R().
		SetContext(ctx).
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp)
	// The Excel client does not identify as Codex.
	if clientID != openai.ExcelClientID {
		authUA, authOriginator := service.CodexCanonicalAuthIdentity()
		request.SetHeader("User-Agent", authUA).SetHeader("originator", authOriginator)
	}
	resp, err := request.Post(s.tokenURLForClient(clientID))

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		if clientID == openai.ExcelClientID {
			return nil, excelOAuthFailure("OPENAI_OAUTH_TOKEN_REFRESH_FAILED", "refresh", resp)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_TOKEN_REFRESH_FAILED", "token refresh failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	return &tokenResp, nil
}

func createOpenAIReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL: proxyURL,
		Timeout:  120 * time.Second,
	})
}

func shouldReturnOpenAINoProxyHint(ctx context.Context, proxyURL string, err error) bool {
	if strings.TrimSpace(proxyURL) != "" || err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

func newOpenAINoProxyHintError(cause error) error {
	return infraerrors.New(
		http.StatusBadGateway,
		"OPENAI_OAUTH_PROXY_REQUIRED",
		"OpenAI OAuth request failed: no proxy is configured and this server could not reach OpenAI directly. Select a proxy that can access OpenAI, then retry; if the authorization code has expired, regenerate the authorization URL.",
	).WithCause(cause)
}
