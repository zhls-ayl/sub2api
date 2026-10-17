package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/gin-gonic/gin"
)

// leonardoTestTimeout 覆盖凭据链路（一次 get-session 交换加一次积分查询）；
// 出图阶段不受它约束（提交后由出图服务脱钩轮询，上限 10 分钟）。
const leonardoTestTimeout = 45 * time.Second

// 测试出图固定用最便宜的档位：gpt-image-2 LOW × 1024x1024，约 8 积分。
const (
	leonardoTestImageQuality = "low"
	leonardoTestImageSize    = "1024x1024"
)

// testLeonardoAccountConnection 测试 Leonardo 账号，与 OpenAI 渠道的出图测试同构：
// 真实出一张图，以 image 事件回显。
//
// Cookie 原生号先走凭据链路（cookie → get-session → JWT → 积分查询，顺带把 JWT、
// cognito_sub 落库），再用与正式请求相同的 LeonardoImageService 出图（最低档，
// 约 8 积分）。中转账号（apikey + base_url）复用 OpenAI Images 的 API Key 测试路径。
func (s *AccountTestService) testLeonardoAccountConnection(
	c *gin.Context, account *Account, modelID, prompt string,
) error {
	testModel := strings.TrimSpace(modelID)
	if testModel == "" {
		if ids := LeonardoModelIDs(); len(ids) > 0 {
			testModel = ids[0]
		}
	}
	if isLeonardoRelayAccount(account) {
		return s.testOpenAIImageAPIKey(c, c.Request.Context(), account, account.GetMappedModel(testModel), resolveLeonardoTestPrompt(prompt))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModel})

	if strings.TrimSpace(account.GetCredential("cookie")) == "" {
		return s.sendErrorAndEnd(c, "Leonardo account has no cookie — paste the cookie exported from a logged-in app.leonardo.ai session")
	}

	images := s.leonardoTestImages()
	authCtx, cancelAuth := context.WithTimeout(c.Request.Context(), leonardoTestTimeout)
	defer cancelAuth()

	s.sendEvent(c, TestEvent{Type: "status", Text: "Exchanging a JWT from the cookie via get-session..."})
	token, err := s.leonardoTestTokenProvider().GetAccessToken(authCtx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, formatLeonardoTestError(err))
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: "JWT obtained. Querying credits balance..."})

	sub := strings.TrimSpace(account.GetCredential("cognito_sub"))
	if sub == "" {
		return s.sendErrorAndEnd(c, "JWT refreshed but cognito_sub is missing — retry the test once the refresh completes")
	}
	credits, err := images.clients.clientForAccount(account).QueryCredits(authCtx, token, sub)
	if err != nil {
		return s.sendErrorAndEnd(c, formatLeonardoTestError(err))
	}

	identity := strings.TrimSpace(account.GetCredential("email"))
	if identity == "" {
		identity = sub
	}
	snapshot, planName := newLeonardoCreditsSnapshot(credits)
	if planName == "" {
		planName = credits.Plan
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: fmt.Sprintf("Account: %s (plan: %s)", identity, planName)})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Credits: " + formatLeonardoCreditPools(snapshot)})

	s.sendEvent(c, TestEvent{Type: "status", Text: fmt.Sprintf(
		"Generating 1 image with %s (quality %s, %s)...", testModel, leonardoTestImageQuality, leonardoTestImageSize)})
	result, err := images.GenerateCall(c.Request.Context(), account, token, NewLeonardoImageCall(&OpenAIImagesRequest{
		Model:   testModel,
		Prompt:  resolveLeonardoTestPrompt(prompt),
		Quality: leonardoTestImageQuality,
		Size:    leonardoTestImageSize,
		N:       1,
	}, ""))
	if err != nil {
		return s.sendErrorAndEnd(c, formatLeonardoTestError(err))
	}

	count, err := s.emitLeonardoTestImages(c, result.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf(
		"generated %d image(s); generation %s charged %d credits\n", count, result.GenerationID, result.APICreditCost)})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// formatLeonardoCreditPools 按池列出积分：Fast 与 Rollover 带上限（套餐未知时
// 只给剩余量），Top-up 没有上限。
func formatLeonardoCreditPools(snapshot *LeonardoCreditsSnapshot) string {
	pool := func(remaining, limit int) string {
		if limit > 0 {
			return fmt.Sprintf("%d/%d", remaining, limit)
		}
		return fmt.Sprintf("%d", remaining)
	}
	fastLabel := "Fast"
	if snapshot.FastDaily {
		fastLabel = "Fast (daily)"
	}
	return fmt.Sprintf("%s %s · Rollover %s · Top-up %d (%d usable for generation)",
		fastLabel, pool(snapshot.Subscription, snapshot.FastLimit),
		pool(snapshot.Rollover, snapshot.RolloverLimit), snapshot.Paid, snapshot.Usable)
}

// emitLeonardoTestImages 把出图服务的 OpenAI 形响应体逐张转成 image 事件。
// 测试实例不转存对象存储，产物是 b64_json；url 形态一并兼容。
func (s *AccountTestService) emitLeonardoTestImages(c *gin.Context, body []byte) (int, error) {
	var parsed struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("failed to parse generated images: %w", err)
	}
	count := 0
	for _, item := range parsed.Data {
		switch {
		case item.B64JSON != "":
			mimeType := leonardoTestImageMimeType(item.B64JSON)
			s.sendEvent(c, TestEvent{Type: "image", ImageURL: "data:" + mimeType + ";base64," + item.B64JSON, MimeType: mimeType})
		case item.URL != "":
			s.sendEvent(c, TestEvent{Type: "image", ImageURL: item.URL})
		default:
			continue
		}
		count++
	}
	if count == 0 {
		return 0, errors.New("leonardo returned no images")
	}
	return count, nil
}

// leonardoTestImageMimeType 按文件头嗅探 base64 图片的类型（Leonardo 产物通常是 JPEG）。
func leonardoTestImageMimeType(b64 string) string {
	prefix := b64
	if len(prefix) > 1024 {
		prefix = prefix[:1024]
	}
	if head, err := base64.StdEncoding.DecodeString(prefix); err == nil {
		if mimeType := http.DetectContentType(head); strings.HasPrefix(mimeType, "image/") {
			return mimeType
		}
	}
	return "image/png"
}

func resolveLeonardoTestPrompt(prompt string) string {
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		return trimmed
	}
	return defaultOpenAIImageTestPrompt
}

// leonardoTestImageService 是账号测试专用的出图服务：不转存对象存储（产物以
// data URL 回显），客户端连接池也与生产实例分开。
var leonardoTestImageService = NewLeonardoImageService(nil)

func (s *AccountTestService) leonardoTestImages() *LeonardoImageService {
	if s.leonardoImageService != nil {
		return s.leonardoImageService
	}
	return leonardoTestImageService
}

// leonardoTestTokenProvider 返回注入的共享 provider；未注入时构造仅进程内
// 加锁的独立实例，落库语义保持一致。
func (s *AccountTestService) leonardoTestTokenProvider() *LeonardoTokenProvider {
	if s.leonardoTokenProvider != nil {
		return s.leonardoTokenProvider
	}
	return NewLeonardoTokenProvider(s.accountRepo, nil)
}

// formatLeonardoTestError 把 pkg 的类型化错误翻译成运维看得懂的处置建议。
func formatLeonardoTestError(err error) string {
	if err == nil {
		return "unknown Leonardo error"
	}
	var (
		indeterminate *leonardoSubmitIndeterminateError
		unavailable   *leonardoResultUnavailableError
		authErr       *leonardopkg.AuthError
		rateErr       *leonardopkg.RateLimitedError
		suspensionErr *leonardopkg.SuspensionError
		insufficient  *leonardopkg.InsufficientTokensError
		contentErr    *leonardopkg.ContentRejectedError
		tempErr       *leonardopkg.UpstreamTemporaryError
		requestErr    *leonardopkg.RequestError
	)
	switch {
	case errors.As(err, &indeterminate):
		return fmt.Sprintf("Leonardo generation submit result is unknown (credits may have been charged); check the account's generation history: %s", err.Error())
	case errors.As(err, &unavailable):
		return fmt.Sprintf("Leonardo accepted the generation but its result could not be retrieved: %s", err.Error())
	case errors.As(err, &suspensionErr):
		return fmt.Sprintf("Leonardo account is suspended for violating the user agreement: %s", err.Error())
	case errors.As(err, &authErr):
		return fmt.Sprintf("Leonardo credentials rejected (%d) — the cookie has likely expired, re-export it from a logged-in app.leonardo.ai session: %s",
			authErr.StatusCode, err.Error())
	case errors.As(err, &insufficient):
		return fmt.Sprintf("Leonardo credits exhausted — the subscription renews periodically: %s", err.Error())
	case errors.As(err, &contentErr):
		return fmt.Sprintf("Leonardo content moderation rejected the test prompt — try a different prompt: %s", err.Error())
	case errors.As(err, &rateErr):
		return fmt.Sprintf("Leonardo rate limited the request — wait a minute and retry: %s", err.Error())
	case errors.As(err, &tempErr):
		return fmt.Sprintf("Leonardo upstream temporarily unavailable, retry later: %s", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return fmt.Sprintf("Leonardo test timed out: %s", err.Error())
	case errors.As(err, &requestErr):
		return requestErr.User()
	default:
		return err.Error()
	}
}
