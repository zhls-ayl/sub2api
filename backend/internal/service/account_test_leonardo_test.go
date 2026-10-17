package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeLeonardoUpstream 模拟 Leonardo GraphQL 与 CDN：积分充足、提交即完成、
// 产物是一张 JPEG。记录 Generate 请求变量与积分查询次数供断言。
// openai/ 前缀的模型（gpt-image-2.5 系列）按抓包返回 apiCreditCost=null +
// cost.amount 字符串。
type fakeLeonardoUpstream struct {
	server        *httptest.Server
	mu            sync.Mutex
	gen           map[string]any
	creditQueries int
	// image 是 CDN 返回的产物字节；默认是一段只有 JPEG 文件头的伪造字节
	// （无法解码），需要真实图片的用例自行替换。
	image []byte
}

func newFakeLeonardoUpstream(t *testing.T) *fakeLeonardoUpstream {
	t.Helper()
	f := &fakeLeonardoUpstream{
		image: append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, make([]byte, 64)...),
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img.jpg" {
			f.mu.Lock()
			image := f.image
			f.mu.Unlock()
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(image)
			return
		}
		var body struct {
			OperationName string         `json:"operationName"`
			Variables     map[string]any `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		switch body.OperationName {
		case "GetUserTokensFromSub":
			f.mu.Lock()
			f.creditQueries++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"user_details":[{"plan":"STANDARD","subscriptionTokens":25000,"paidTokens":0,"rolloverTokens":0}]}}`))
		case "Generate":
			request, _ := body.Variables["request"].(map[string]any)
			f.mu.Lock()
			f.gen = request
			f.mu.Unlock()
			if model, _ := request["model"].(string); strings.HasPrefix(model, "openai/") {
				_, _ = w.Write([]byte(`{"data":{"generate":{"generationId":"gen-test","apiCreditCost":null,"cost":{"amount":"13","unit":"CREDITS"}}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"generate":{"generationId":"gen-test","apiCreditCost":8}}}`))
		case "GetAIGenerationFeedStatuses":
			_, _ = w.Write([]byte(`{"data":{"generations":[{"id":"gen-test","status":"COMPLETE"}]}}`))
		case "GetGenerationDetail":
			// 按提交的 quantity 返回对应张数。
			f.mu.Lock()
			params, _ := f.gen["parameters"].(map[string]any)
			f.mu.Unlock()
			quantity := 1
			if q, ok := params["quantity"].(float64); ok && q > 0 {
				quantity = int(q)
			}
			images := make([]string, 0, quantity)
			for i := 0; i < quantity; i++ {
				images = append(images, fmt.Sprintf(`{"id":"img-%d","url":"%s/img.jpg"}`, i, f.server.URL))
			}
			_, _ = w.Write([]byte(`{"data":{"generations":[{"id":"gen-test","status":"COMPLETE","imageWidth":1024,"imageHeight":1024,"generated_images":[` + strings.Join(images, ",") + `]}]}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLeonardoUpstream) imageService() *LeonardoImageService {
	svc := NewLeonardoImageService(nil)
	svc.clients.options = leonardopkg.Options{GraphQLURL: f.server.URL + "/graphql", HTTPClient: f.server.Client()}
	return svc
}

func TestLeonardoCookieAccountTestGeneratesImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newFakeLeonardoUpstream(t)
	svc := &AccountTestService{leonardoImageService: upstream.imageService()}
	account := &Account{ID: 81, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{
		"cookie":       "__Secure-better-auth.session_token=x",
		"access_token": "jwt",
		"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"cognito_sub":  "sub-1",
	}}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/81/test", nil)

	require.NoError(t, svc.testLeonardoAccountConnection(c, account, "gpt-image-2", "a red fox"))

	out := w.Body.String()
	require.Contains(t, out, `"type":"image"`)
	require.Contains(t, out, `data:image/jpeg;base64,`)
	require.Contains(t, out, `"type":"test_complete"`)
	require.Contains(t, out, "gen-test")
	require.Contains(t, out, "plan: Premium")
	require.Contains(t, out, "Fast 25000/25000 · Rollover 0/75000 · Top-up 0")
	require.NotContains(t, out, `"type":"error"`)

	// 测试出图走最低档：LOW × 1024x1024，提示词透传。
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Equal(t, "gpt-image-2", upstream.gen["model"])
	params, _ := upstream.gen["parameters"].(map[string]any)
	require.Equal(t, "LOW", params["quality"])
	require.EqualValues(t, 1024, params["width"])
	require.EqualValues(t, 1024, params["height"])
	require.Equal(t, "a red fox", params["prompt"])
}

// gpt-image-2.5 系列：对外名换成 openai/ 上游 slug 提交，提示词优化固定 OFF；
// 出图流程不再查积分（只有账号测试自身的那一次），实耗取 cost.amount。
func TestLeonardoCookieAccountTestGeneratesFlareImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := newFakeLeonardoUpstream(t)
	svc := &AccountTestService{leonardoImageService: upstream.imageService()}
	account := &Account{ID: 82, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{
		"cookie":       "__Secure-better-auth.session_token=x",
		"access_token": "jwt",
		"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"cognito_sub":  "sub-1",
	}}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/82/test", nil)

	require.NoError(t, svc.testLeonardoAccountConnection(c, account, "gpt-image-2.5-flare", "a red fox"))

	out := w.Body.String()
	require.Contains(t, out, `"type":"image"`)
	require.Contains(t, out, "charged 13 credits")
	require.NotContains(t, out, `"type":"error"`)

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Equal(t, "openai/gpt-image-2.5-flare", upstream.gen["model"])
	params, _ := upstream.gen["parameters"].(map[string]any)
	require.Equal(t, "OFF", params["prompt_enhance"])
	require.Equal(t, 1, upstream.creditQueries, "出图流程不应再做积分预估查询")
}

func TestLeonardoTestImageMimeTypeSniffing(t *testing.T) {
	require.Equal(t, "image/jpeg", leonardoTestImageMimeType("/9j/4AAQSkZJRgABAQ=="))
	require.Equal(t, "image/png", leonardoTestImageMimeType("not-base64!"))
	require.True(t, strings.HasPrefix(leonardoTestImageMimeType("iVBORw0KGgoAAAANSUhEUg=="), "image/png"))
}

// 透明背景：只有客户端明确传 background=transparent 时才带 transparency=true。
func TestLeonardoGenerateCallTransparencyFollowsBackground(t *testing.T) {
	for _, tc := range []struct {
		background string
		want       bool
	}{
		{background: "transparent", want: true},
		{background: " Transparent ", want: true},
		{background: "opaque", want: false},
		{background: "", want: false},
	} {
		upstream := newFakeLeonardoUpstream(t)
		account := &Account{ID: 83, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
		_, err := upstream.imageService().GenerateCall(context.Background(), account, "jwt", NewLeonardoImageCall(&OpenAIImagesRequest{
			Model: "gpt-image-2.5-flare", Prompt: "an apple", Background: tc.background, N: 1,
		}, ""))
		require.NoError(t, err, tc.background)

		upstream.mu.Lock()
		params, _ := upstream.gen["parameters"].(map[string]any)
		value, present := params["transparency"]
		upstream.mu.Unlock()
		require.Equal(t, tc.want, present, "background=%q", tc.background)
		if tc.want {
			require.Equal(t, true, value)
		}
	}
}

// n>1：原生号按 quantity=n 一次提交，返回并按实际张数计费。
func TestLeonardoGenerateCallMultipleImages(t *testing.T) {
	upstream := newFakeLeonardoUpstream(t)
	account := &Account{ID: 84, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	result, err := upstream.imageService().GenerateCall(context.Background(), account, "jwt", NewLeonardoImageCall(&OpenAIImagesRequest{
		Model: "gpt-image-2", Prompt: "an apple", N: 3,
	}, ""))
	require.NoError(t, err)
	require.Equal(t, 3, result.Forward.ImageCount)

	var body struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(result.Body, &body))
	require.Len(t, body.Data, 3)

	upstream.mu.Lock()
	params, _ := upstream.gen["parameters"].(map[string]any)
	upstream.mu.Unlock()
	require.EqualValues(t, 3, params["quantity"])
}

// 质量档：gpt-image-2.5 系列放开 XHIGH / MAX；gpt-image-2 传 max 直接 400，不打上游。
func TestLeonardoGenerateCallExtendedQuality(t *testing.T) {
	upstream := newFakeLeonardoUpstream(t)
	account := &Account{ID: 85, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	_, err := upstream.imageService().GenerateCall(context.Background(), account, "jwt", NewLeonardoImageCall(&OpenAIImagesRequest{
		Model: "gpt-image-2.5-flare", Prompt: "an apple", Quality: "xhigh", N: 1,
	}, ""))
	require.NoError(t, err)
	upstream.mu.Lock()
	params, _ := upstream.gen["parameters"].(map[string]any)
	upstream.gen = nil
	upstream.mu.Unlock()
	require.Equal(t, "XHIGH", params["quality"])

	_, err = upstream.imageService().GenerateCall(context.Background(), account, "jwt", NewLeonardoImageCall(&OpenAIImagesRequest{
		Model: "gpt-image-2", Prompt: "an apple", Quality: "max", N: 1,
	}, ""))
	var requestErr *leonardopkg.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Contains(t, requestErr.User(), "only supported by gpt-image-2.5")
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Nil(t, upstream.gen, "不支持的质量档不应发出上游请求")
}
