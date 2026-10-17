package leonardo

import "testing"

func TestCapQuality(t *testing.T) {
	cases := []struct{ quality, max, want string }{
		{QualityHigh, "", QualityHigh},
		{QualityHigh, "medium", QualityMedium},
		{QualityXHigh, " MEDIUM ", QualityMedium},
		{QualityMax, "xhigh", QualityXHigh},
		{QualityLow, "medium", QualityLow},
		{QualityMedium, "medium", QualityMedium},
		{QualityHigh, "bogus", QualityHigh},
	}
	for _, tc := range cases {
		if got := CapQuality(tc.quality, tc.max); got != tc.want {
			t.Fatalf("CapQuality(%q, %q) = %q, want %q", tc.quality, tc.max, got, tc.want)
		}
	}
}

func TestPlanImageUpscaleSource(t *testing.T) {
	limits := ImageLimits{MaxShortEdge: 1024, MaxUpscaleFactor: 2}
	cases := []struct {
		size           string
		srcW, srcH     int
		wantUpscaled   bool
		limitsOverride *ImageLimits
	}{
		{size: "2048x2048", srcW: 1024, srcH: 1024, wantUpscaled: true},
		// 2880² 的 1024² 需要放大 2.81 倍，超过 2 倍上限，退到同比例的 1536²。
		{size: "2880x2880", srcW: 1536, srcH: 1536, wantUpscaled: true},
		{size: "3584x2016", srcW: 2048, srcH: 1152, wantUpscaled: true},
		{size: "2048x1536", srcW: 1152, srcH: 864, wantUpscaled: true},
		// 目标不在上游清单里，但有同比例的出图尺寸：照样能交付。
		{size: "1920x1080", srcW: 1536, srcH: 864, wantUpscaled: true},
		// 本身不是精确比例，清单里没有同比例的更小尺寸：按原尺寸出图。
		{size: "2048x1136", srcW: 2048, srcH: 1136},
		// 短边不超过上限：不放大。
		{size: "1536x1024", srcW: 1536, srcH: 1024},
		{size: "auto", srcW: 1024, srcH: 1024},
		// 不设上限：不放大。
		{size: "2048x2048", srcW: 2048, srcH: 2048, limitsOverride: &ImageLimits{}},
		// 放大倍数上限 0 → 默认 2。
		{size: "2880x2880", srcW: 1536, srcH: 1536, wantUpscaled: true, limitsOverride: &ImageLimits{MaxShortEdge: 1024}},
		// 放大倍数上限 3：2880² 可以直接用 1024²。
		{size: "2880x2880", srcW: 1024, srcH: 1024, wantUpscaled: true, limitsOverride: &ImageLimits{MaxShortEdge: 1024, MaxUpscaleFactor: 3}},
	}
	for _, tc := range cases {
		l := limits
		if tc.limitsOverride != nil {
			l = *tc.limitsOverride
		}
		plan, err := PlanImage(ImageOptions{Size: tc.size, Limits: l})
		if err != nil {
			t.Fatalf("PlanImage(%q) error: %v", tc.size, err)
		}
		if plan.Source.Width != tc.srcW || plan.Source.Height != tc.srcH {
			t.Fatalf("PlanImage(%q) source = %dx%d, want %dx%d", tc.size, plan.Source.Width, plan.Source.Height, tc.srcW, tc.srcH)
		}
		if plan.Upscaled() != tc.wantUpscaled {
			t.Fatalf("PlanImage(%q) upscaled = %v, want %v", tc.size, plan.Upscaled(), tc.wantUpscaled)
		}
		// 出图尺寸与交付尺寸比例精确相等，放大时不拉伸、不裁剪。
		if plan.Source.Width*plan.Target.Height != plan.Source.Height*plan.Target.Width {
			t.Fatalf("PlanImage(%q) source %dx%d changes the aspect ratio", tc.size, plan.Source.Width, plan.Source.Height)
		}
	}
}

func TestPlanImageQuality(t *testing.T) {
	plan, err := PlanImage(ImageOptions{Size: "1024x1024", Quality: "high", Limits: ImageLimits{MaxQuality: "medium"}})
	if err != nil {
		t.Fatalf("PlanImage error: %v", err)
	}
	if plan.RequestedQuality != QualityHigh || plan.Quality != QualityMedium {
		t.Fatalf("quality = requested %q / actual %q, want HIGH / MEDIUM", plan.RequestedQuality, plan.Quality)
	}

	// 请求档位本身仍按模型能力校验：gpt-image-2 不支持 xhigh。
	if _, err := PlanImage(ImageOptions{Size: "1024x1024", Quality: "xhigh", Limits: ImageLimits{MaxQuality: "medium"}}); err == nil {
		t.Fatal("xhigh on gpt-image-2 should still be rejected")
	}
}

func TestBuildImagePlanRequestUsesSourceAndCappedQuality(t *testing.T) {
	req, plan, err := BuildImagePlanRequest(ImageOptions{
		Prompt:  "a cat",
		Size:    "2048x2048",
		Quality: "high",
		Limits:  ImageLimits{MaxQuality: "medium", MaxShortEdge: 1024},
	})
	if err != nil {
		t.Fatalf("BuildImagePlanRequest error: %v", err)
	}
	if req.Parameters.Width != 1024 || req.Parameters.Height != 1024 || req.Parameters.Quality != QualityMedium {
		t.Fatalf("params = %dx%d %s, want 1024x1024 MEDIUM", req.Parameters.Width, req.Parameters.Height, req.Parameters.Quality)
	}
	if plan.Target != (ImageSlot{Width: 2048, Height: 2048}) || plan.RequestedQuality != QualityHigh {
		t.Fatalf("plan = %+v", plan)
	}
}
