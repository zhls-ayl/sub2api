package leonardo

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 尺寸原样透传：不折算到任何预设矩阵；宽高是否在上游允许值清单内交给上游。
func TestResolveImageSlotPassesSizeThrough(t *testing.T) {
	cases := []struct {
		name   string
		size   string
		width  int
		height int
	}{
		{"square", "1024x1024", 1024, 1024},
		{"landscape standard", "1536x1024", 1536, 1024},
		{"portrait standard", "1024x1536", 1024, 1536},
		{"dall-e wide", "1792x1024", 1792, 1024},
		{"2k 16:9", "2048x1152", 2048, 1152},
		{"largest square", "2880x2880", 2880, 2880},
		// 1080 不在上游高度清单里：本地放行，由上游 VALIDATION_ERROR 拒绝。
		{"left to upstream validation", "1920x1080", 1920, 1080},
		{"auto", "auto", 1024, 1024},
		{"empty", "", 1024, 1024},
		{"unicode times separator", "1024×1024", 1024, 1024},
		{"upper-case separator", " 1536X1024 ", 1536, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slot, err := ResolveImageSlot(tc.size)
			if err != nil {
				t.Fatalf("ResolveImageSlot(%q) error: %v", tc.size, err)
			}
			if slot.Width != tc.width || slot.Height != tc.height {
				t.Fatalf("ResolveImageSlot(%q) = %dx%d, want %dx%d", tc.size, slot.Width, slot.Height, tc.width, tc.height)
			}
		})
	}
}

func TestResolveImageSlotRejectsInvalid(t *testing.T) {
	for _, size := range []string{"bogus", "1024", "ax1024", "0x1024", "1024x0", "-5x100"} {
		if _, err := ResolveImageSlot(size); err == nil {
			t.Fatalf("ResolveImageSlot(%q) expected error", size)
		}
	}
}

// 上游宽高清单都放行、但超出 OpenAI 像素 / 比例限制的组合，本地直接 400：
// 提交上去会扣积分后在生成阶段 FAILED（3808x3584 实测）。
func TestResolveImageSlotRejectsOpenAILimits(t *testing.T) {
	cases := map[string]string{
		"3808x3584": "total pixels",
		"768x768":   "total pixels",
		"768x640":   "total pixels",
		"3808x1024": "aspect ratio",
		"640x2048":  "aspect ratio",
	}
	for size, want := range cases {
		_, err := ResolveImageSlot(size)
		var reqErr *RequestError
		if !errors.As(err, &reqErr) {
			t.Fatalf("ResolveImageSlot(%q) = %v, want RequestError", size, err)
		}
		if !strings.Contains(reqErr.User(), want) {
			t.Fatalf("ResolveImageSlot(%q) message = %q, want it to mention %q", size, reqErr.User(), want)
		}
	}
	// 边界值本身合法：3840x2160 正好 8,294,400 像素、1024x640 正好 655,360 像素、
	// 3072x1024 正好 3:1。
	for _, size := range []string{"3840x2160", "1024x640", "3072x1024"} {
		if _, err := ResolveImageSlot(size); err != nil {
			t.Fatalf("ResolveImageSlot(%q) boundary should pass, got %v", size, err)
		}
	}
}

func TestNormalizeQuality(t *testing.T) {
	cases := map[string]string{
		"":       QualityMedium,
		"auto":   QualityMedium,
		"AUTO":   QualityMedium,
		"low":    QualityLow,
		"Medium": QualityMedium,
		"HIGH":   QualityHigh,
	}
	for _, extended := range []bool{false, true} {
		for input, want := range cases {
			got, err := NormalizeQuality(input, extended)
			if err != nil {
				t.Fatalf("NormalizeQuality(%q, %v) error: %v", input, extended, err)
			}
			if got != want {
				t.Fatalf("NormalizeQuality(%q, %v) = %q, want %q", input, extended, got, want)
			}
		}
	}

	// gpt-image-2 只有三档：xhigh / max 明确拒绝。
	for _, input := range []string{"xhigh", "max"} {
		if _, err := NormalizeQuality(input, false); err == nil || !strings.Contains(err.Error(), "only supported by gpt-image-2.5") {
			t.Fatalf("NormalizeQuality(%q, false) should reject with a 2.5-only message, got %v", input, err)
		}
	}
	// gpt-image-2.5 系列额外支持 XHIGH / MAX（大小写不敏感）。
	for input, want := range map[string]string{"xhigh": QualityXHigh, "MAX": QualityMax, "Max": QualityMax} {
		got, err := NormalizeQuality(input, true)
		if err != nil || got != want {
			t.Fatalf("NormalizeQuality(%q, true) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := NormalizeQuality("ultra", true); err == nil {
		t.Fatal("unknown quality must be rejected")
	}
}

func TestBuildImageRequestExtendedQuality(t *testing.T) {
	req, err := BuildImageRequest(ImageOptions{Prompt: "apple", Quality: "xhigh", ExtendedQuality: true})
	if err != nil {
		t.Fatalf("BuildImageRequest error: %v", err)
	}
	if req.Parameters.Quality != QualityXHigh {
		t.Fatalf("quality = %q, want XHIGH", req.Parameters.Quality)
	}
	if _, err := BuildImageRequest(ImageOptions{Prompt: "apple", Quality: "xhigh"}); err == nil {
		t.Fatal("xhigh without extended quality must be rejected")
	}
}

func TestBuildImageRequestTextToImage(t *testing.T) {
	req, err := BuildImageRequest(ImageOptions{
		UpstreamModel: GPTImage2Model,
		Prompt:        "  a red panda barista  ",
		Quality:       "high",
		Size:          "1024x1024",
	})
	if err != nil {
		t.Fatalf("BuildImageRequest error: %v", err)
	}
	if req.Model != "gpt-image-2" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.Public {
		t.Fatal("public should default to false")
	}
	if req.Parameters.Prompt != "a red panda barista" {
		t.Fatalf("prompt = %q", req.Parameters.Prompt)
	}
	if req.Parameters.Quantity != 1 {
		t.Fatalf("quantity = %d", req.Parameters.Quantity)
	}
	if req.Parameters.Quality != QualityHigh {
		t.Fatalf("quality = %q", req.Parameters.Quality)
	}
	if req.Parameters.PromptEnhance != "OFF" {
		t.Fatalf("prompt_enhance = %q", req.Parameters.PromptEnhance)
	}
	if len(req.Parameters.StyleIDs) != 1 || req.Parameters.StyleIDs[0] != StyleNoneID {
		t.Fatalf("style_ids = %v", req.Parameters.StyleIDs)
	}
	if req.Parameters.Guidances != nil {
		t.Fatalf("text-to-image must not carry guidances")
	}
	// 固定字段序列化断言（字段集合与取值）。
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parameters, ok := decoded["parameters"].(map[string]any)
	if !ok {
		t.Fatal("parameters missing")
	}
	if _, ok := parameters["width"]; !ok {
		t.Fatal("parameters.width missing")
	}
	if _, ok := parameters["guidances"]; ok {
		t.Fatal("guidances should be omitted for text-to-image")
	}
}

func TestBuildImageRequestReferences(t *testing.T) {
	req, err := BuildImageRequest(ImageOptions{
		Prompt:            "edit this",
		Size:              "1536x1024",
		ReferenceImageIDs: []string{"id-1", "id-2"},
	})
	if err != nil {
		t.Fatalf("BuildImageRequest error: %v", err)
	}
	if req.Parameters.Guidances == nil || len(req.Parameters.Guidances.ImageReference) != 2 {
		t.Fatalf("guidances missing")
	}
	first := req.Parameters.Guidances.ImageReference[0]
	if first.Image.ID != "id-1" || first.Image.Type != GuidanceUploadedType || first.Strength != GuidanceStrengthMID {
		t.Fatalf("guidance[0] = %+v", first)
	}
	if req.Parameters.Width != 1536 || req.Parameters.Height != 1024 {
		t.Fatalf("1536x1024 should pass through unchanged, got %dx%d", req.Parameters.Width, req.Parameters.Height)
	}
}

func TestBuildImageRequestReferenceLimit(t *testing.T) {
	ids := make([]string, 7)
	for i := range ids {
		ids[i] = "id"
	}
	if _, err := BuildImageRequest(ImageOptions{Prompt: "p", ReferenceImageIDs: ids}); err == nil {
		t.Fatal("expected error for 7 references")
	}
}

func TestBuildImageRequestRequiresPrompt(t *testing.T) {
	if _, err := BuildImageRequest(ImageOptions{Prompt: "   "}); err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestErrorClassification(t *testing.T) {
	suspension := classifyGraphQLErrorMessage("You are suspended for violating our user agreement.")
	var suspensionErr *SuspensionError
	if !errors.As(suspension, &suspensionErr) {
		t.Fatalf("suspension string should classify as SuspensionError, got %T", suspension)
	}

	insufficient := classifyGraphQLErrorMessage("insufficient tokens for this request")
	var insufficientErr *InsufficientTokensError
	if !errors.As(insufficient, &insufficientErr) {
		t.Fatalf("insufficient tokens should classify as InsufficientTokensError, got %T", insufficient)
	}

	auth := classifyGraphQLErrorMessage("JWT invalid or expired")
	var authErr *AuthError
	if !errors.As(auth, &authErr) {
		t.Fatalf("jwt message should classify as AuthError, got %T", auth)
	}

	terminal := classifyGraphQLErrorMessage("some random upstream complaint")
	var requestErr *RequestError
	if !errors.As(terminal, &requestErr) {
		t.Fatalf("unmatched message should stay RequestError, got %T", terminal)
	}
}

func TestClassifyFailureErrorCode(t *testing.T) {
	var contentErr *ContentRejectedError
	if err := ClassifyFailureErrorCode(&GenerationFailure{ErrorCode: "PROVIDER_MODERATION_ERROR"}); !errors.As(err, &contentErr) {
		t.Fatalf("moderation code should map to ContentRejectedError, got %T", err)
	}
	if err := ClassifyFailureErrorCode(&GenerationFailure{NSFW: true}); !errors.As(err, &contentErr) {
		t.Fatalf("nsfw flag should map to ContentRejectedError, got %T", err)
	}
	var tempErr *UpstreamTemporaryError
	if err := ClassifyFailureErrorCode(&GenerationFailure{ErrorCode: "PROVIDER_TIMEOUT"}); !errors.As(err, &tempErr) {
		t.Fatalf("provider timeout should map to UpstreamTemporaryError, got %T", err)
	}
	var authErr *AuthError
	if err := ClassifyFailureErrorCode(&GenerationFailure{ErrorCode: "PROVIDER_AUTHENTICATION_ERROR"}); !errors.As(err, &authErr) {
		t.Fatalf("provider auth should map to AuthError, got %T", err)
	}
	var requestErr *RequestError
	if err := ClassifyFailureErrorCode(&GenerationFailure{ErrorCode: "PROVIDER_INVALID_REQUEST"}); !errors.As(err, &requestErr) {
		t.Fatalf("invalid request should stay terminal RequestError, got %T", err)
	}
}

func TestBuildImageRequestTransparency(t *testing.T) {
	transparent, err := BuildImageRequest(ImageOptions{Prompt: "apple", Transparent: true})
	if err != nil {
		t.Fatalf("BuildImageRequest error: %v", err)
	}
	body, _ := json.Marshal(transparent)
	if !strings.Contains(string(body), `"transparency":true`) {
		t.Fatalf("transparent request must carry transparency=true: %s", body)
	}

	opaque, err := BuildImageRequest(ImageOptions{Prompt: "apple"})
	if err != nil {
		t.Fatalf("BuildImageRequest error: %v", err)
	}
	body, _ = json.Marshal(opaque)
	if strings.Contains(string(body), "transparency") {
		t.Fatalf("opaque request must omit transparency: %s", body)
	}
}

func TestBuildImageRequestQuantity(t *testing.T) {
	for _, tc := range []struct {
		quantity int
		want     int
	}{{0, 1}, {1, 1}, {3, 3}, {MaxImageQuantity, MaxImageQuantity}} {
		req, err := BuildImageRequest(ImageOptions{Prompt: "apple", Quantity: tc.quantity})
		if err != nil {
			t.Fatalf("quantity %d: %v", tc.quantity, err)
		}
		if req.Parameters.Quantity != tc.want {
			t.Fatalf("quantity %d => %d, want %d", tc.quantity, req.Parameters.Quantity, tc.want)
		}
	}
	if _, err := BuildImageRequest(ImageOptions{Prompt: "apple", Quantity: MaxImageQuantity + 1}); err == nil {
		t.Fatalf("quantity above %d must be rejected", MaxImageQuantity)
	}
}
