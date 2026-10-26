package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// newFitTestPNG 生成指定尺寸的纯色 PNG（区别于 1x1 的 fishTestPNG）。
func newFitTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func decodeFitImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode fitted image: %v", err)
	}
	_ = format
	return img
}

func fitPlan(t *testing.T, size string, limits domain.FishFitLimits) *domain.FishSizePlan {
	t.Helper()
	plan, err := domain.FishPlanSize(size, limits)
	if err != nil {
		t.Fatalf("FishPlanSize(%q): %v", size, err)
	}
	return plan
}

func TestFitCoverImageExactRatioDownscale(t *testing.T) {
	plan := fitPlan(t, "816x816", domain.FishFitLimits{})
	out, err := fitCoverImage(newFitTestPNG(t, 1024, 1024), plan, "", nil)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	img := decodeFitImage(t, out)
	if img.Bounds().Dx() != 816 || img.Bounds().Dy() != 816 {
		t.Fatalf("output = %dx%d, want 816x816", img.Bounds().Dx(), img.Bounds().Dy())
	}
	if sniffImageOutputFormat(out) != imageOutputFormatPNG {
		t.Fatalf("format = %q, want png（空格式保持原图）", sniffImageOutputFormat(out))
	}
}

func TestFitCoverImageTinyCropDownscale(t *testing.T) {
	// 1536x2048 → 880x1168：先对称裁原生高约 9px 再缩小。
	plan := fitPlan(t, "880x1168", domain.FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2})
	out, err := fitCoverImage(newFitTestPNG(t, 1536, 2048), plan, "", nil)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	img := decodeFitImage(t, out)
	if img.Bounds().Dx() != 880 || img.Bounds().Dy() != 1168 {
		t.Fatalf("output = %dx%d, want 880x1168", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestFitCoverImageUpscaleThenCrop(t *testing.T) {
	// 768x1024 → 880x1168：先放大到 cover（880x1173）再居中裁 5px。
	plan := fitPlan(t, "880x1168", domain.FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2})
	out, err := fitCoverImage(newFitTestPNG(t, 768, 1024), plan, "", nil)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	img := decodeFitImage(t, out)
	if img.Bounds().Dx() != 880 || img.Bounds().Dy() != 1168 {
		t.Fatalf("output = %dx%d, want 880x1168", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestFitCoverImagePureUpscaleNoCrop(t *testing.T) {
	plan := fitPlan(t, "2048x2048", domain.FishFitLimits{MaxCropPixels: 0, MaxUpscaleFactor: 2})
	out, err := fitCoverImage(newFitTestPNG(t, 1024, 1024), plan, "", nil)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	img := decodeFitImage(t, out)
	if img.Bounds().Dx() != 2048 || img.Bounds().Dy() != 2048 {
		t.Fatalf("output = %dx%d, want 2048x2048", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestFitCoverImageAlreadyTargetIsNoop(t *testing.T) {
	// 上游实际交付即客户目标（如原生档偏离计划）：零变换原样返回。
	src := newFitTestPNG(t, 816, 816)
	plan := fitPlan(t, "816x816", domain.FishFitLimits{})
	out, err := fitCoverImage(src, plan, "", nil)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	if !bytes.Equal(src, out) {
		t.Fatal("上游交付即目标时应原样返回字节")
	}
}

func TestFitCoverImageAlreadyTargetStillTranscodes(t *testing.T) {
	// 上游交付即目标但客户要 jpeg：不裁不缩，仍须转码。
	plan := fitPlan(t, "816x816", domain.FishFitLimits{})
	out, err := fitCoverImage(newFitTestPNG(t, 816, 816), plan, "jpeg", nil)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	if sniffImageOutputFormat(out) != imageOutputFormatJPEG {
		t.Fatalf("format = %q, want jpeg", sniffImageOutputFormat(out))
	}
}

func TestFitCoverImageRefusesCropBeyondPlan(t *testing.T) {
	// 规划 1344x576 → 1568x672（同比例、零裁剪），上游却给了 1:1 图：按实际
	// 尺寸重算要裁掉大半高度，必须报错回退，而不是悄悄多裁。
	plan := fitPlan(t, "1568x672", domain.DefaultFishFitLimits())
	if plan.Native != "1344x576" || plan.CropPixels != 0 {
		t.Fatalf("plan = %+v, want native 1344x576 without crop", plan)
	}
	if _, err := fitCoverImage(newFitTestPNG(t, 1024, 1024), plan, "", nil); err == nil {
		t.Fatal("off-ratio upstream image must not be cropped beyond the plan")
	}
}

func TestFitCoverImageJPEGOutput(t *testing.T) {
	plan := fitPlan(t, "880x1168", domain.FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2})
	compression := 90
	out, err := fitCoverImage(newFitTestPNG(t, 1536, 2048), plan, "jpeg", &compression)
	if err != nil {
		t.Fatalf("fitCoverImage: %v", err)
	}
	if sniffImageOutputFormat(out) != imageOutputFormatJPEG {
		t.Fatalf("format = %q, want jpeg", sniffImageOutputFormat(out))
	}
	img := decodeFitImage(t, out)
	if img.Bounds().Dx() != 880 || img.Bounds().Dy() != 1168 {
		t.Fatalf("output = %dx%d, want 880x1168", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestFitCoverImagesFallbackAndPassthrough(t *testing.T) {
	good := newFitTestPNG(t, 1536, 2048)
	plan := fitPlan(t, "880x1168", domain.FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2})

	// nil / 无变换 plan：原样返回同一切片
	if got := fitCoverImages([][]byte{good}, nil, "", nil); &got[0][0] != &good[0] {
		t.Fatal("nil plan 应原样返回")
	}
	exact := fitPlan(t, "1024x1024", domain.FishFitLimits{})
	if got := fitCoverImages([][]byte{good}, exact, "", nil); &got[0][0] != &good[0] {
		t.Fatal("无变换 plan 应原样返回")
	}

	// 坏字节回退：不报错、坏图字节保留（回退路径格式转码对 png 源是 no-op）
	broken := []byte("not-an-image")
	got := fitCoverImages([][]byte{broken, good}, plan, "", nil)
	if !bytes.Equal(got[0], broken) {
		t.Fatal("坏图应回退原图字节")
	}
	if img := decodeFitImage(t, got[1]); img.Bounds().Dx() != 880 || img.Bounds().Dy() != 1168 {
		t.Fatalf("好图应完成变换, got %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestCenterCropSymmetricSplit(t *testing.T) {
	// 10px 宽裁掉 3（奇数）：左 1 右 2，结果宽 7。
	img := image.NewRGBA(image.Rect(0, 0, 10, 1))
	for x := 0; x < 10; x++ {
		img.SetRGBA(x, 0, color.RGBA{R: uint8(x), A: 255})
	}
	cropped := cropHorizontal(img, 3)
	b := cropped.Bounds()
	if b.Dx() != 7 || b.Dy() != 1 {
		t.Fatalf("bounds = %v, want 宽7高1", b)
	}
	// SubImage 共享像素：x=1..7 可见，左侧保留的是原 x=1（R=1），右侧到原 x=7（R=7）。
	if got := cropped.At(b.Min.X, b.Min.Y).(color.RGBA).R; got != 1 {
		t.Fatalf("左缘像素 R=%d, want 1（裁左1）", got)
	}
	if got := cropped.At(b.Max.X-1, b.Min.Y).(color.RGBA).R; got != 7 {
		t.Fatalf("右缘像素 R=%d, want 7（裁右2）", got)
	}
}
