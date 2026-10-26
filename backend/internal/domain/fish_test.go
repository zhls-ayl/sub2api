package domain

import (
	"slices"
	"testing"
)

func TestFishFitLimitsNormalized(t *testing.T) {
	cases := []struct {
		name       string
		in         FishFitLimits
		wantCrop   int
		wantFactor float64
	}{
		{"零值=不裁剪+默认放大", FishFitLimits{}, 0, 2.0},
		{"负裁剪=未配置取默认", FishFitLimits{MaxCropPixels: -1}, FishFitDefaultMaxCropPixels, 2.0},
		{"裁剪钳上限", FishFitLimits{MaxCropPixels: 999}, FishFitMaxCropPixelsHardLimit, 2.0},
		{"因子零取默认", FishFitLimits{MaxUpscaleFactor: 0}, 0, 2.0},
		{"因子下钳到1", FishFitLimits{MaxUpscaleFactor: 0.5}, 0, 1.0},
		{"因子上钳", FishFitLimits{MaxUpscaleFactor: 9}, 0, FishFitMaxUpscaleHardLimit},
	}
	for _, tc := range cases {
		got := tc.in.Normalized()
		if got.MaxCropPixels != tc.wantCrop || got.MaxUpscaleFactor != tc.wantFactor {
			t.Fatalf("%s: Normalized() = {%d, %v}, want {%d, %v}",
				tc.name, got.MaxCropPixels, got.MaxUpscaleFactor, tc.wantCrop, tc.wantFactor)
		}
	}
}

func TestFishPlanSize(t *testing.T) {
	defaults := DefaultFishFitLimits()
	cases := []struct {
		name         string
		size         string
		limits       *FishFitLimits
		wantErr      bool
		wantTarget   string
		wantNative   string
		wantAxis     int
		wantCrop     int
		wantNoChange bool // NeedsTransform 为假（原生档即目标）
	}{
		{name: "空串默认1024", size: "", wantTarget: "1024x1024", wantNative: "1024x1024", wantAxis: FishCropNone, wantNoChange: true},
		{name: "白名单精确档", size: "2048x1152", wantTarget: "2048x1152", wantNative: "2048x1152", wantAxis: FishCropNone, wantNoChange: true},
		{name: "同比例缩小", size: "816x816", wantTarget: "816x816", wantNative: "1024x1024", wantAxis: FishCropNone},
		{name: "同比例放大", size: "1728x2304", limits: &FishFitLimits{MaxCropPixels: 0, MaxUpscaleFactor: 2.0}, wantTarget: "1728x2304", wantNative: "1536x2048", wantAxis: FishCropNone},
		{
			name: "微裁通过且缩小优先", size: "880x1168",
			limits:     &FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2.0},
			wantTarget: "880x1168", wantNative: "1536x2048", wantAxis: FishCropHeight, wantCrop: 5,
		},
		{name: "裁量超限拒绝", size: "880x1168", wantErr: true}, // 5.33px > 默认4
		{name: "禁放大时拒绝放大档", size: "1728x2304", limits: &FishFitLimits{MaxCropPixels: 0, MaxUpscaleFactor: 1.0}, wantErr: true},
		{name: "零裁剪只留精确比例", size: "896x1184", limits: &FishFitLimits{MaxCropPixels: 0, MaxUpscaleFactor: 2.0}, wantErr: true},
		{name: "零裁剪仍允许缩小", size: "816x816", limits: &FishFitLimits{MaxCropPixels: 0, MaxUpscaleFactor: 1.0}, wantTarget: "816x816", wantNative: "1024x1024", wantAxis: FishCropNone},
		{name: "比例缺口过大拒绝", size: "3000x1000", wantErr: true},
		{name: "auto不可解析走中转", size: "auto", wantErr: true},
		{name: "垃圾串", size: "big", wantErr: true},
		{name: "缺高", size: "1024x", wantErr: true},
		{name: "零尺寸", size: "0x0", wantErr: true},
		{
			name: "同裁量时f就近", size: "1024x1008",
			limits:     &FishFitLimits{MaxCropPixels: 16, MaxUpscaleFactor: 2.0},
			wantTarget: "1024x1008", wantNative: "1024x1024", wantAxis: FishCropHeight, wantCrop: 16,
		},
		{
			name: "横版微裁", size: "1360x768",
			limits:     &FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2.0},
			wantTarget: "1360x768", wantNative: "2048x1152", wantAxis: FishCropWidth, wantCrop: 5,
		},
	}
	for _, tc := range cases {
		limits := defaults
		if tc.limits != nil {
			limits = *tc.limits
			if tc.limits.MaxUpscaleFactor == 0 {
				t.Fatalf("%s: 测试 limits 的放大因子不应为零（零值语义是默认2.0），请显式给值", tc.name)
			}
		}
		plan, err := FishPlanSize(tc.size, limits)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: FishPlanSize(%q) 意外成功: %+v", tc.name, tc.size, plan)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: FishPlanSize(%q) 报错: %v", tc.name, tc.size, err)
		}
		if plan.TargetSize != tc.wantTarget || plan.Native != tc.wantNative ||
			plan.CropAxis != tc.wantAxis || plan.CropPixels != tc.wantCrop {
			t.Fatalf("%s: plan = {target %s, native %s, axis %d, crop %d}, want {%s, %s, %d, %d}",
				tc.name, plan.TargetSize, plan.Native, plan.CropAxis, plan.CropPixels,
				tc.wantTarget, tc.wantNative, tc.wantAxis, tc.wantCrop)
		}
		if plan.NeedsTransform() == tc.wantNoChange {
			t.Fatalf("%s: NeedsTransform() = %v, want %v", tc.name, plan.NeedsTransform(), !tc.wantNoChange)
		}
		if !slices.Contains(FishPixelSizes, plan.Native) {
			t.Fatalf("%s: native %q 不在白名单", tc.name, plan.Native)
		}
	}
}

// TestFishPlanSizeInvariants 对一批尺寸做不变式扫描：成功规划时必须满足
// 放大倍数与裁量都在限额内、native 在白名单内。
func TestFishPlanSizeInvariants(t *testing.T) {
	limits := FishFitLimits{MaxCropPixels: 6, MaxUpscaleFactor: 1.8}.Normalized()
	sizes := []string{"", "1024x1024", "816x816", "880x1168", "1168x880", "1024x1536",
		"1536x1024", "1920x1080", "720x1280", "1760x2352", "2480x3312", "3840x2160",
		"999x999", "123x456", "4064x2032"}
	for _, size := range sizes {
		plan, err := FishPlanSize(size, limits)
		if err != nil {
			continue
		}
		if plan.ScaleFactor > limits.MaxUpscaleFactor+1e-6 {
			t.Fatalf("%q: factor %v 超限 %v", size, plan.ScaleFactor, limits.MaxUpscaleFactor)
		}
		if plan.CropPixels > limits.MaxCropPixels+1 {
			t.Fatalf("%q: crop %d 超限 %d", size, plan.CropPixels, limits.MaxCropPixels)
		}
		if !slices.Contains(FishPixelSizes, plan.Native) {
			t.Fatalf("%q: native %q 不在白名单", size, plan.Native)
		}
		if plan.TargetWidth < 1 || plan.TargetHeight < 1 {
			t.Fatalf("%q: 非法目标 %dx%d", size, plan.TargetWidth, plan.TargetHeight)
		}
	}
}

// TestFishPlanSizeGPTImageMaxSize 校验 gpt-image 交付尺寸上限：长边 ≤ 3840、
// 总像素 ≤ 8,294,400。超限的 size 规划失败（由调用方交给中转号）。
func TestFishPlanSizeGPTImageMaxSize(t *testing.T) {
	limits := FishFitLimits{MaxCropPixels: 64, MaxUpscaleFactor: FishFitMaxUpscaleHardLimit}
	for _, size := range []string{"3840x2160", "2160x3840", "2880x2880"} {
		if _, err := FishPlanSize(size, limits); err != nil {
			t.Fatalf("%s 在上限内应可规划: %v", size, err)
		}
	}
	for _, size := range []string{"3856x2160", "2160x3856", "2896x2896", "5760x5760", "8640x8640"} {
		if plan, err := FishPlanSize(size, limits); err == nil {
			t.Fatalf("%s 超出 gpt-image 上限应规划失败，得到 %+v", size, plan)
		}
	}
}
