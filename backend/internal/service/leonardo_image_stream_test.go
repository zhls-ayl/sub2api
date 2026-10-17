package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestLeonardoImagesStreamEventsPerImage(t *testing.T) {
	result := &LeonardoImageResult{
		Body:         []byte(`{"created":1700000000,"data":[{"b64_json":"QUJD"},{"b64_json":"REVG"}]}`),
		Size:         "1024x1024",
		OutputFormat: "png",
		Quality:      "xhigh",
		Background:   "transparent",
	}
	events := LeonardoImagesStreamEvents(&OpenAIImagesRequest{Model: "gpt-image-2.5-flare", Stream: true}, result)
	require.Len(t, events, 2)
	for i, want := range []string{"QUJD", "REVG"} {
		event := events[i]
		require.Equal(t, "image_generation.completed", event.Name)
		require.Equal(t, "image_generation.completed", gjson.GetBytes(event.Payload, "type").String())
		require.Equal(t, want, gjson.GetBytes(event.Payload, "b64_json").String())
		require.EqualValues(t, 1700000000, gjson.GetBytes(event.Payload, "created_at").Int())
		require.Equal(t, "1024x1024", gjson.GetBytes(event.Payload, "size").String())
		require.Equal(t, "png", gjson.GetBytes(event.Payload, "output_format").String())
		require.Equal(t, "xhigh", gjson.GetBytes(event.Payload, "quality").String())
		require.Equal(t, "transparent", gjson.GetBytes(event.Payload, "background").String())
		require.Equal(t, "gpt-image-2.5-flare", gjson.GetBytes(event.Payload, "model").String())
		require.False(t, gjson.GetBytes(event.Payload, "url").Exists())
	}
}

func TestLeonardoImagesStreamEventsEditsAndURLs(t *testing.T) {
	// 编辑请求用 image_edit 前缀；response_format=url 时附 data URI。
	edit := LeonardoImagesStreamEvents(&OpenAIImagesRequest{Endpoint: openAIImagesEditsEndpoint, ResponseFormat: "url"}, &LeonardoImageResult{
		Body:         []byte(`{"data":[{"b64_json":"QUJD"}]}`),
		OutputFormat: "jpeg",
	})
	require.Len(t, edit, 1)
	require.Equal(t, "image_edit.completed", edit[0].Name)
	require.Equal(t, "data:image/jpeg;base64,QUJD", gjson.GetBytes(edit[0].Payload, "url").String())

	// 对象存储转存后只有 url：b64_json 留空，url 填存储地址；空条目跳过。
	stored := LeonardoImagesStreamEvents(&OpenAIImagesRequest{}, &LeonardoImageResult{
		Body: []byte(`{"data":[{"url":"https://cdn.example.com/a.png"},{}]}`),
	})
	require.Len(t, stored, 1)
	require.Equal(t, "", gjson.GetBytes(stored[0].Payload, "b64_json").String())
	require.Equal(t, "https://cdn.example.com/a.png", gjson.GetBytes(stored[0].Payload, "url").String())

	require.Nil(t, LeonardoImagesStreamEvents(&OpenAIImagesRequest{}, nil))
}

func TestImagesStreamErrorPayload(t *testing.T) {
	payload := ImagesStreamErrorPayload("image_generation_user_error", "moderation_blocked", "Content flagged by provider moderation")
	require.Equal(t, "error", gjson.GetBytes(payload, "type").String())
	require.Equal(t, "image_generation_user_error", gjson.GetBytes(payload, "error.type").String())
	require.Equal(t, "moderation_blocked", gjson.GetBytes(payload, "error.code").String())
	require.Equal(t, "Content flagged by provider moderation", gjson.GetBytes(payload, "error.message").String())
	require.Equal(t, gjson.Null, gjson.GetBytes(payload, "error.param").Type)

	// 无 code 时 code 键仍在，值为 null。
	payload = ImagesStreamErrorPayload("api_error", "", "Upstream request failed")
	require.True(t, gjson.GetBytes(payload, "error.code").Exists())
	require.Equal(t, gjson.Null, gjson.GetBytes(payload, "error.code").Type)
	require.True(t, gjson.GetBytes(payload, "error.param").Exists())
}

func TestValidateLeonardoImageParamsAcceptsStreamAndPartialImages(t *testing.T) {
	partials := 2
	require.NoError(t, validateLeonardoImageParams(&OpenAIImagesRequest{Prompt: "cat", Stream: true, PartialImages: &partials}))
	require.Error(t, validateLeonardoImageParams(&OpenAIImagesRequest{Prompt: "cat", HasMask: true}))
	require.Error(t, validateLeonardoImageParams(&OpenAIImagesRequest{Prompt: "cat", N: 9}))
}

// 出图结果带上假流式事件需要的元数据：实际尺寸、按文件头判定的格式、质量、背景。
func TestLeonardoGenerateCallResultMetadata(t *testing.T) {
	upstream := newFakeLeonardoUpstream(t)
	account := &Account{ID: 86, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	result, err := upstream.imageService().GenerateCall(context.Background(), account, "jwt", NewLeonardoImageCall(&OpenAIImagesRequest{
		Model: "gpt-image-2", Prompt: "an apple", Quality: "low", N: 2, Stream: true,
	}, ""))
	require.NoError(t, err)
	require.Equal(t, "1024x1024", result.Size)
	require.Equal(t, "jpeg", result.OutputFormat)
	require.Equal(t, "low", result.Quality)
	require.Equal(t, "opaque", result.Background)
	require.Len(t, LeonardoImagesStreamEvents(&OpenAIImagesRequest{Stream: true}, result), 2)

	// 非流式响应体最外层回显实际出图参数（与 OpenAI gpt-image 响应一致）。
	require.Equal(t, "opaque", gjson.GetBytes(result.Body, "background").String())
	require.Equal(t, "jpeg", gjson.GetBytes(result.Body, "output_format").String())
	require.Equal(t, "low", gjson.GetBytes(result.Body, "quality").String())
	require.Equal(t, "1024x1024", gjson.GetBytes(result.Body, "size").String())
	// 未配置换算比例：不带 usage，用量记录里也没有 token。
	require.Nil(t, result.Usage)
	require.False(t, gjson.GetBytes(result.Body, "usage").Exists())
	require.Equal(t, OpenAIUsage{KiroCredits: 8}, result.Forward.Usage, "积分照常记录，与 usage 估算开关无关")
}

// usage 按扣除积分 × 比例估算：非流式写在最外层，流式只挂在最后一个 completed 事件上。
func TestLeonardoGenerateCallEstimatesUsage(t *testing.T) {
	upstream := newFakeLeonardoUpstream(t)
	account := &Account{ID: 88, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}}
	call := NewLeonardoImageCall(&OpenAIImagesRequest{
		Model: "gpt-image-2", Prompt: "an apple on a table", Quality: "low", N: 2, Stream: true,
	}, "")
	call.UsageTokensPerCredit = 34
	result, err := upstream.imageService().GenerateCall(context.Background(), account, "jwt", call)
	require.NoError(t, err)

	// 假上游 gpt-image-2 的 apiCreditCost = 8 → 8 × 34 = 272。
	usage := gjson.GetBytes(result.Body, "usage")
	require.True(t, usage.Exists())
	require.Equal(t, int64(272), usage.Get("output_tokens").Int())
	require.Equal(t, int64(272), usage.Get("output_tokens_details.image_tokens").Int())
	textTokens := usage.Get("input_tokens_details.text_tokens").Int()
	require.Positive(t, textTokens)
	require.Equal(t, textTokens, usage.Get("input_tokens").Int())
	require.Equal(t, textTokens+272, usage.Get("total_tokens").Int())

	// 估算值同时写进用量记录（与 OpenAI 出图同一套映射：OutputTokens 含图片 token）。
	require.Equal(t, int(textTokens), result.Forward.Usage.InputTokens)
	require.Equal(t, 272, result.Forward.Usage.OutputTokens)
	require.Equal(t, 272, result.Forward.Usage.ImageOutputTokens)
	require.Zero(t, result.Forward.Usage.ImageInputTokens)
	// 实扣积分写进上游积分列（usage_logs.kiro_credits），供今日统计估算成本。
	require.Equal(t, 8.0, result.Forward.Usage.KiroCredits)

	events := LeonardoImagesStreamEvents(call.Request, result)
	require.Len(t, events, 2)
	require.False(t, gjson.GetBytes(events[0].Payload, "usage").Exists(), "只有最后一个事件带 usage")
	require.Equal(t, int64(272), gjson.GetBytes(events[1].Payload, "usage.output_tokens").Int())
}

func TestLeonardoImageUsage(t *testing.T) {
	require.Nil(t, leonardoImageUsage("gpt-image-2", "cat", 8, 0, 1), "比例为 0 不返回 usage")
	require.Nil(t, leonardoImageUsage("gpt-image-2", "cat", 0, 34, 1), "积分未知不返回 usage")
	usage := leonardoImageUsage("gpt-image-2", "", 137, 34, 1)
	require.Equal(t, int64(4658), gjson.GetBytes(usage, "output_tokens").Int())
	require.Equal(t, int64(0), gjson.GetBytes(usage, "input_tokens").Int())
	require.Equal(t, int64(0), gjson.GetBytes(usage, "input_tokens_details.image_tokens").Int())
}

// 账号设了质量与尺寸上限：按上限出图（1024² MEDIUM），本地等比超分回客户尺寸
// （2048²）；回显请求档位，按交付尺寸计费，usage 按尺寸与质量倍数折回，积分如实记录。
func TestLeonardoGenerateCallAppliesAccountImageLimits(t *testing.T) {
	upstream := newFakeLeonardoUpstream(t)
	upstream.image = encodeTestJPEG(t, 1024, 1024, 90)
	account := &Account{ID: 89, Platform: PlatformLeonardo, Type: AccountTypeOAuth, Credentials: map[string]any{"cookie": "c"}, Extra: map[string]any{
		"leonardo_max_quality":    "medium",
		"leonardo_max_short_edge": 1024,
	}}
	call := NewLeonardoImageCall(&OpenAIImagesRequest{
		Model: "gpt-image-2", Prompt: "an apple", Quality: "high", Size: "2048x2048",
	}, "")
	call.UsageTokensPerCredit = 34

	result, err := upstream.imageService().GenerateCall(context.Background(), account, "jwt", call)
	require.NoError(t, err)

	upstream.mu.Lock()
	params, _ := upstream.gen["parameters"].(map[string]any)
	upstream.mu.Unlock()
	require.EqualValues(t, 1024, params["width"])
	require.EqualValues(t, 1024, params["height"])
	require.Equal(t, "MEDIUM", params["quality"])

	b64 := gjson.GetBytes(result.Body, "data.0.b64_json").String()
	decoded, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(decoded))
	require.NoError(t, err)
	require.Equal(t, []any{2048, 2048, "jpeg"}, []any{cfg.Width, cfg.Height, format})

	require.Equal(t, "2048x2048", result.Size)
	require.Equal(t, "2048x2048", gjson.GetBytes(result.Body, "size").String())
	require.Equal(t, "high", result.Quality)
	require.Equal(t, "2K", result.Forward.ImageSize)
	require.Equal(t, 8.0, result.Forward.Usage.KiroCredits, "积分记实扣值，不放大")

	// 8 积分 × 34 × 交付/出图像素 4 × HIGH/MEDIUM 质量倍数 4160/1056。
	wantTokens := int(math.Round(8 * 34 * 4 * (4160.0 / 1056.0)))
	require.Equal(t, int64(wantTokens), gjson.GetBytes(result.Body, "usage.output_tokens").Int())
	require.Equal(t, wantTokens, result.Forward.Usage.OutputTokens)
}
