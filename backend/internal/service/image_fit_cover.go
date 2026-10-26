package service

import (
	"bytes"
	"fmt"
	"image"
	"log/slog"
	"math"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"golang.org/x/image/draw"
)

// fitCoverImages 把每张成品图按 plan 裁剪缩放回客户请求尺寸（plan 为空或
// 原生档即目标时原样返回）。逐张串行；单张失败保留原图并记日志——产物已经
// 生成（上游已扣费），变换失败不该让整个请求失败（镜像 upscaleImages）。
// 回显与计费的交付尺寸由调用方按实际字节读取（fishDeliveredSize）。
func fitCoverImages(images [][]byte, plan *domain.FishSizePlan, format string, compression *int) [][]byte {
	if plan == nil || !plan.NeedsTransform() {
		return images
	}
	out := make([][]byte, len(images))
	for i, data := range images {
		fitted, err := fitCoverImage(data, plan, format, compression)
		if err != nil || len(fitted) == 0 {
			slog.Warn("fish_image_fit_failed",
				"index", i, "target", plan.TargetSize, "native", plan.Native, "error", err)
			// 回退原图但仍按请求格式转码（镜像 upscaleImages 的失败语义）。
			out[i] = applyImageOutputFormat([][]byte{data}, format, compression)[0]
			continue
		}
		out[i] = fitted
	}
	return out
}

// fitCoverImage 按客户目标尺寸做一次 cover 裁剪缩放（画质优先的执行顺序）：
//
//   - 缩小（f≤1）：先在源分辨率对称居中裁（奇数像素两侧差 1），再 Lanczos3
//     缩到精确目标——裁剪发生在信息量最大的位置，缩放是最后一步。
//   - 放大（f>1）：先对完整原图 Lanczos3 放大到 cover 尺寸（所有原始像素参与
//     插值），再在目标分辨率对称居中裁掉溢出——丢的是插值边缘而非原始信息。
//
// 源图实际尺寸可能与 plan.Native 有偏差，倍数与裁量按解码后的实际尺寸重算；
// 重算后的裁量超出规划（上游比例偏离计划）时报错回退原图，绝不多裁。
// format 为空时保持原图格式（识别不出按 png）；jpeg 质量取 output_compression。
func fitCoverImage(data []byte, plan *domain.FishSizePlan, format string, compression *int) ([]byte, error) {
	if len(data) == 0 || plan == nil || plan.TargetWidth <= 0 || plan.TargetHeight <= 0 {
		return nil, fmt.Errorf("invalid fish fit input")
	}
	imageUpscaleSlots <- struct{}{}
	defer func() { <-imageUpscaleSlots }()

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	srcW, srcH := img.Bounds().Dx(), img.Bounds().Dy()
	if srcW == plan.TargetWidth && srcH == plan.TargetHeight {
		// 上游交付即目标：不裁不缩，但仍按请求的 output_format 转码。
		return transcodeImage(data, format, compression)
	}
	f := math.Max(float64(plan.TargetWidth)/float64(srcW), float64(plan.TargetHeight)/float64(srcH))
	overflow := math.Max(f*float64(srcW)-float64(plan.TargetWidth), f*float64(srcH)-float64(plan.TargetHeight))
	if overflow > float64(plan.CropPixels)+1 {
		return nil, fmt.Errorf("upstream image %dx%d needs a %.1fpx crop to reach %s; plan allows %dpx",
			srcW, srcH, overflow, plan.TargetSize, plan.CropPixels)
	}
	if format == "" {
		format = sniffImageOutputFormat(data)
		if format == "" {
			format = imageOutputFormatPNG
		}
	}
	jpegQuality := upscaleJPEGDefaultQuality
	if compression != nil {
		jpegQuality = imageJPEGQuality(compression)
	}

	if f <= 1 {
		return fitCoverDownscale(img, plan.TargetWidth, plan.TargetHeight, f, format, jpegQuality)
	}
	return fitCoverUpscale(img, plan.TargetWidth, plan.TargetHeight, f, format, jpegQuality)
}

// fitCoverDownscale 先裁后缩：cover 溢出换算回源分辨率取整、对称均分，
// 再 Lanczos3 缩到精确目标。
func fitCoverDownscale(img image.Image, targetW, targetH int, f float64, format string, jpegQuality int) ([]byte, error) {
	bounds := img.Bounds()
	if crop := cropFromOverflow(f*float64(bounds.Dx())-float64(targetW), f); crop > 0 && crop < bounds.Dx() {
		img = cropHorizontal(img, crop)
	}
	if crop := cropFromOverflow(f*float64(bounds.Dy())-float64(targetH), f); crop > 0 && crop < bounds.Dy() {
		img = cropVertical(img, crop)
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	lanczos3Kernel.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
	return encodeImage(dst, format, jpegQuality)
}

// fitCoverUpscale 先缩后裁：整图放大到 cover 尺寸（向上取整保证不小于目标），
// 再在目标分辨率对称居中裁掉取整引入的溢出。
func fitCoverUpscale(img image.Image, targetW, targetH int, f float64, format string, jpegQuality int) ([]byte, error) {
	scaledW := ceilAtLeast(f*float64(img.Bounds().Dx()), targetW)
	scaledH := ceilAtLeast(f*float64(img.Bounds().Dy()), targetH)
	scaled := image.NewRGBA(image.Rect(0, 0, scaledW, scaledH))
	lanczos3Kernel.Scale(scaled, scaled.Bounds(), img, img.Bounds(), draw.Src, nil)
	dx, dy := scaledW-targetW, scaledH-targetH
	cropped := centerCrop(scaled, dx/2, dy/2, scaledW-(dx-dx/2), scaledH-(dy-dy/2))
	return encodeImage(cropped, format, jpegQuality)
}

// cropFromOverflow 把目标分辨率的 cover 溢出换算回源分辨率并取整（仅正值）。
func cropFromOverflow(overflow, f float64) int {
	if overflow <= domain.FishFitEpsilon {
		return 0
	}
	return int(math.Round(overflow / f))
}

// ceilAtLeast 向上取整且不小于 floor。
func ceilAtLeast(value float64, floor int) int {
	rounded := int(math.Ceil(value - domain.FishFitEpsilon))
	if rounded < floor {
		return floor
	}
	return rounded
}

// centerCrop 返回 (x0,y0)-(x1,y1) 子图（绝对坐标，相对 img.Bounds()）。
// 罕见的不支持 SubImage 的解码结果返回原图（后续缩放仍执行，仅少裁）。
func centerCrop(img image.Image, x0, y0, x1, y1 int) image.Image {
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	cropped, ok := img.(subImager)
	if !ok {
		return img
	}
	return cropped.SubImage(image.Rect(x0, y0, x1, y1))
}

// cropHorizontal / cropVertical 对称居中裁掉 total 像素（奇数时两侧差 1）。
func cropHorizontal(img image.Image, total int) image.Image {
	b := img.Bounds()
	left := total / 2
	return centerCrop(img, b.Min.X+left, b.Min.Y, b.Max.X-(total-left), b.Max.Y)
}

func cropVertical(img image.Image, total int) image.Image {
	b := img.Bounds()
	top := total / 2
	return centerCrop(img, b.Min.X, b.Min.Y+top, b.Max.X, b.Max.Y-(total-top))
}
