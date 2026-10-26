package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// TestFishImagesServiceUnavailable 覆盖 handler 的空服务守卫（DI 异常时的
// 显式 404，而不是 panic）。
func TestFishImagesServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &GatewayHandler{} // fishImageService 为 nil
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{}`))
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 1})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1})
	h.FishImages(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not_found_error") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestFishPlatformListedInModels 断言 fish 平台的对外模型清单进入
// defaultModelIDsForPlatform。
func TestFishPlatformListedInModels(t *testing.T) {
	ids := defaultModelIDsForPlatform(service.PlatformFish)
	if len(ids) == 0 {
		t.Fatal("fish model ids empty")
	}
	joined := strings.Join(ids, ",")
	for _, want := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		if !strings.Contains(joined, want) {
			t.Errorf("models %s missing %q", joined, want)
		}
	}
}
