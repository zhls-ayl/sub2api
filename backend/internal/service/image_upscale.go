package service

import (
	"bytes"
	"fmt"
	"image"
	"log/slog"
	"math"
	"runtime"

	"golang.org/x/image/draw"
)

// upscaleJPEGDefaultQuality 是超分后重新编码 JPEG 的默认质量（客户未传
// output_compression 时）：95 与 100 肉眼几乎无差，体积小得多。
const upscaleJPEGDefaultQuality = 95

// lanczos3Kernel 是 Lanczos3 重采样核（与 sharp / Pillow 的 LANCZOS 一致）。
var lanczos3Kernel = &draw.Kernel{Support: 3, At: lanczos3}

func lanczos3(t float64) float64 {
	if t < 0 {
		t = -t
	}
	if t == 0 {
		return 1
	}
	if t >= 3 {
		return 0
	}
	x := math.Pi * t
	return 3 * math.Sin(x) * math.Sin(x/3) / (x * x)
}

// imageUpscaleSlots 限制进程内同时进行的超分数量（解码 + 重采样 + 编码都吃 CPU，
// 2880x2880 的 RGBA 中间图约 33MB），避免并发出图时把 CPU 和内存瞬间打满。
var imageUpscaleSlots = make(chan struct{}, max(1, runtime.GOMAXPROCS(0)))

// upscaleImages 把每张图等比超分到 width x height 并按 format 编码（format 为空时
// 保持原图格式）。逐张串行；单张失败时退回原尺寸（仍按 format 转码）并记日志——
// 产物已经生成（上游已扣费），超分失败不该让整个请求失败，交付尺寸由调用方按
// 实际字节读取。
func upscaleImages(images [][]byte, width, height int, format string, compression *int) [][]byte {
	out := make([][]byte, len(images))
	for i, data := range images {
		upscaled, err := upscaleImage(data, width, height, format, compression)
		if err != nil || len(upscaled) == 0 {
			slog.Warn("image_upscale_failed", "index", i, "target", fmt.Sprintf("%dx%d", width, height), "error", err)
			out[i] = applyImageOutputFormat([][]byte{data}, format, compression)[0]
			continue
		}
		out[i] = upscaled
	}
	return out
}

// upscaleImage 用 Lanczos3 把一张图等比放大到 width x height。只接受与目标比例精确
// 相等的原图（不拉伸、不裁剪）；format 为空时保持原图格式（识别不出按 png），
// jpeg 质量取 output_compression，未传时取 upscaleJPEGDefaultQuality。透明通道保留。
func upscaleImage(data []byte, width, height int, format string, compression *int) ([]byte, error) {
	if len(data) == 0 || width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid upscale input")
	}
	imageUpscaleSlots <- struct{}{}
	defer func() { <-imageUpscaleSlots }()

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	bounds := img.Bounds()
	if bounds.Dx()*height != bounds.Dy()*width {
		return nil, fmt.Errorf("aspect ratio mismatch: %dx%d -> %dx%d", bounds.Dx(), bounds.Dy(), width, height)
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

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	lanczos3Kernel.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)
	return encodeImage(dst, format, jpegQuality)
}
