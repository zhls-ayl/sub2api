package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Leonardo 是纯出图平台：文本协议端点必须在选号前拦截，否则 Leonardo 账号的
// JWT / 中转 API Key 会被当作 Claude 凭据转发出去。
func TestGatewayRoutesLeonardoRejectsTextEndpoints(t *testing.T) {
	router := newGatewayRoutesTestRouter(service.PlatformLeonardo)

	for _, path := range []string{
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/messages/count_tokens",
		"/v1/chat/completions",
		"/chat/completions",
		"/v1/responses",
		"/v1/responses/compact",
		"/responses",
		"/backend-api/codex/responses",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-image-2"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code, "path=%s", path)
		require.Contains(t, w.Body.String(), "only supports the Images API", "path=%s", path)
	}
}

func TestGatewayRoutesLeonardoImagesPathsAreNotRejected(t *testing.T) {
	router := newGatewayRoutesTestRouter(service.PlatformLeonardo)

	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a cat"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotContains(t, w.Body.String(), "only supports the Images API", "path=%s", path)
		require.NotContains(t, w.Body.String(), "Images API is not supported for this platform", "path=%s", path)
	}
}
