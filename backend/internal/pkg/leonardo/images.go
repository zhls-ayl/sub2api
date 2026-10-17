package leonardo

import (
	"fmt"
	"strconv"
	"strings"
)

// GPTImage2Model 是 gpt-image-2 的上游 slug（BuildImageRequest 的默认模型）。
// 对外名到上游 slug 的完整对照见 domain.LeonardoModels。
const GPTImage2Model = "gpt-image-2"

// StyleNoneID 是 gpt-image 系列请求固定的 "None" style（关闭 Leonardo 风格）。
const StyleNoneID = "556c1ee5-ec38-42e8-955a-1e82dad0ffa1"

// quality 档位：gpt-image-2 支持 LOW/MEDIUM/HIGH；gpt-image-2.5 系列额外支持
// XHIGH/MAX（2026-10 抓包确认）。auto 折到默认 MEDIUM。
const (
	QualityLow    = "LOW"
	QualityMedium = "MEDIUM"
	QualityHigh   = "HIGH"
	QualityXHigh  = "XHIGH"
	QualityMax    = "MAX"
)

// OpenAI gpt-image-2 的输出尺寸限制（OpenAI 图片生成指南）。Leonardo 的
// Generate 校验只认宽、高各自的允许值清单，组合超出这些限制时仍会受理、
// 生成阶段才 FAILED（2026-10 实测：3808x3584 超上限、768x768 低于下限都是
// 受理后 FAILED），所以网关在本地先拦。
const (
	minImagePixels      = 655_360
	maxImagePixels      = 8_294_400
	maxImageAspectRatio = 3
)

// ImageSlot 是一次解析后的目标尺寸（原样透传给上游的 width / height）。
type ImageSlot struct {
	Width  int
	Height int
}

// ImageOptions 是构造 Generate 请求的输入。
type ImageOptions struct {
	// UpstreamModel 是上游 slug（gpt-image-2 / openai/gpt-image-2.5-*）。
	UpstreamModel string
	Prompt        string
	// Quality：LOW / MEDIUM / HIGH；空或 auto 折 MEDIUM。
	Quality string
	// Size 是 OpenAI Images 的 size 入参（"auto" 或 "WxH"），即交付尺寸。未触发
	// Limits 时原样透传，宽高是否在上游允许值清单内由上游校验；触发时由
	// PlanImage 挑一个同比例的更小出图尺寸。
	Size string
	// ReferenceImageIDs 是已上传过审的 init image id（1-6 张，有序）。
	ReferenceImageIDs []string
	// ExtendedQuality 为 true 时允许 XHIGH / MAX（gpt-image-2.5 系列）。
	ExtendedQuality bool
	// Quantity 是一次生成的张数（OpenAI Images 的 n）；≤0 按 1，上限 MaxImageQuantity。
	Quantity int
	// Transparent 为 true 时请求透明背景。透明效果受提示词影响：提示词里描述
	// 背景会得到不理想的结果（上游行为，网关不改写提示词）。
	Transparent bool
	// Limits 是账号级的质量与尺寸上限（零值 = 不限），见 PlanImage。
	Limits ImageLimits
}

// NormalizeQuality 归一化 quality 入参（大小写不敏感；空与 auto → MEDIUM）。
// extended 为 true 时（gpt-image-2.5 系列）额外接受 xhigh / max；不支持的档位
// 返回明确的 400。
func NormalizeQuality(quality string, extended bool) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(quality))
	switch normalized {
	case "", "AUTO":
		return QualityMedium, nil
	case QualityLow, QualityMedium, QualityHigh:
		return normalized, nil
	case QualityXHigh, QualityMax:
		if extended {
			return normalized, nil
		}
		return "", NewRequestError(fmt.Sprintf("quality %s is only supported by gpt-image-2.5 models", strings.ToLower(normalized)))
	default:
		expected := "low, medium or high"
		if extended {
			expected = "low, medium, high, xhigh or max"
		}
		return "", NewRequestError(fmt.Sprintf("unsupported leonardo image quality: %s (expected %s)", quality, expected))
	}
}

// ResolveImageSlot 把 OpenAI Images 的 size 参数解析为上游 width / height：
//  1. auto / 空 → 1024x1024
//  2. 其余 WxH 原样透传，不折算。上游对宽、高各有一份允许值清单（2026-10
//     实测，宽高可自由组合），清单外的值由上游以 VALIDATION_ERROR 拒绝（不扣
//     积分），报错原文带清单回给客户端；网关不写死清单，上游调整时自动跟上。
//  3. 本地只拦上游校验放行、生成阶段才失败的组合：总像素超出
//     [minImagePixels, maxImagePixels] 或长短边比超过 3:1。
func ResolveImageSlot(size string) (*ImageSlot, error) {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(size, "×", "x")))
	if normalized == "" || normalized == "auto" {
		return &ImageSlot{Width: 1024, Height: 1024}, nil
	}
	width, height, err := parseSize(normalized)
	if err != nil {
		return nil, NewRequestError(fmt.Sprintf("unsupported image size %q: %v", size, err))
	}
	if width <= 0 || height <= 0 {
		return nil, NewRequestError(fmt.Sprintf("unsupported image size %q", size))
	}
	if err := checkImageDimensions(width, height); err != nil {
		return nil, err
	}
	return &ImageSlot{Width: width, Height: height}, nil
}

// checkImageDimensions 检查 OpenAI 的像素与比例限制（宽高须为正数）。
func checkImageDimensions(width, height int) error {
	if pixels := width * height; pixels < minImagePixels || pixels > maxImagePixels {
		return NewRequestError(fmt.Sprintf(
			"unsupported image size %dx%d: total pixels must be between %d and %d (got %d)",
			width, height, minImagePixels, maxImagePixels, pixels))
	}
	longEdge, shortEdge := width, height
	if shortEdge > longEdge {
		longEdge, shortEdge = shortEdge, longEdge
	}
	if longEdge > maxImageAspectRatio*shortEdge {
		return NewRequestError(fmt.Sprintf(
			"unsupported image size %dx%d: aspect ratio must not exceed %d:1", width, height, maxImageAspectRatio))
	}
	return nil
}

// MaxImageQuantity 是一次 Generate 的张数上限（网页版官方上限 8）。
const MaxImageQuantity = 8

// BuildImageRequest 构造 gpt-image 系列的 Generate mutation request 变量。
// 参考图超过 6 张、张数超过 MaxImageQuantity 返回错误（上游上限）。
func BuildImageRequest(opts ImageOptions) (*GenerateRequest, error) {
	request, _, err := BuildImagePlanRequest(opts)
	return request, err
}

// BuildImagePlanRequest 同 BuildImageRequest，额外返回尺寸与质量决策：
// width / height 用 plan.Source，quality 用 plan.Quality。
func BuildImagePlanRequest(opts ImageOptions) (*GenerateRequest, *ImagePlan, error) {
	upstreamModel := strings.TrimSpace(opts.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = GPTImage2Model
	}
	plan, err := PlanImage(opts)
	if err != nil {
		return nil, nil, err
	}
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return nil, nil, NewRequestError("prompt is required")
	}
	if len(opts.ReferenceImageIDs) > 6 {
		return nil, nil, NewRequestError("leonardo image models accept at most 6 image references")
	}
	quantity := opts.Quantity
	if quantity <= 0 {
		quantity = 1
	}
	if quantity > MaxImageQuantity {
		return nil, nil, NewRequestError(fmt.Sprintf("leonardo image models accept at most %d images per request", MaxImageQuantity))
	}

	params := GenerateParams{
		Width:         plan.Source.Width,
		Height:        plan.Source.Height,
		Prompt:        prompt,
		Quantity:      quantity,
		Quality:       plan.Quality,
		PromptEnhance: "OFF",
		StyleIDs:      []string{StyleNoneID},
		Transparency:  opts.Transparent,
	}
	if len(opts.ReferenceImageIDs) > 0 {
		guidances := &GenerateGuidances{}
		for _, id := range opts.ReferenceImageIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				return nil, nil, NewRequestError("reference image id is empty")
			}
			guidance := ImageReferenceGuidance{Strength: GuidanceStrengthMID}
			guidance.Image.ID = id
			guidance.Image.Type = GuidanceUploadedType
			guidances.ImageReference = append(guidances.ImageReference, guidance)
		}
		params.Guidances = guidances
	}
	// public 默认关闭：leo_proxy 的 gpt-image-2 构造器用 public=true，但与其
	// 它图片模型（nano 系 public=false）不一致；公开发布非生成所需。
	return &GenerateRequest{Model: upstreamModel, Public: false, Parameters: params}, plan, nil
}

// UploadExtensionForContentType 按内容类型推断上传扩展名。
func UploadExtensionForContentType(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp":
		return "bmp"
	case "image/tiff":
		return "tiff"
	default:
		return "jpg"
	}
}

func parseSize(normalized string) (int, int, error) {
	widthPart, heightPart, ok := strings.Cut(normalized, "x")
	if !ok {
		return 0, 0, fmt.Errorf("expected WxH")
	}
	width, err := strconv.Atoi(strings.TrimSpace(widthPart))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid width")
	}
	height, err := strconv.Atoi(strings.TrimSpace(heightPart))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid height")
	}
	return width, height, nil
}
