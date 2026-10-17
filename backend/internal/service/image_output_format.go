package service

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"strings"
)

// 本机转码支持的 output_format（OpenAI Images 取值）。webp 没有 CGo-free 的
// 标准库编码器，暂不支持。
const (
	imageOutputFormatPNG  = "png"
	imageOutputFormatJPEG = "jpeg"
	imageOutputFormatWebP = "webp"

	imageJPEGDefaultQuality = 100
)

// normalizeImageOutputFormat 归一化 output_format 入参（大小写不敏感，jpg 视为
// jpeg）。空值返回 ""，表示保持上游格式；不支持的值 ok=false。
func normalizeImageOutputFormat(raw string) (format string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", true
	case imageOutputFormatPNG:
		return imageOutputFormatPNG, true
	case imageOutputFormatJPEG, "jpg":
		return imageOutputFormatJPEG, true
	default:
		return "", false
	}
}

// isImageOutputFormatWebP 报告 output_format 是否为 webp（大小写不敏感）。
func isImageOutputFormatWebP(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), imageOutputFormatWebP)
}

// applyImageOutputFormat 把每张图转成 format（"" 时原样返回）。逐张串行转码，
// 避免多张大图同时解码叠加内存峰值；单张失败时保留原图并记日志——产物已经
// 生成（上游已扣费），转码失败不该让整个请求失败。
func applyImageOutputFormat(images [][]byte, format string, compression *int) [][]byte {
	if format == "" {
		return images
	}
	out := make([][]byte, len(images))
	for i, data := range images {
		converted, err := transcodeImage(data, format, compression)
		if err != nil || len(converted) == 0 {
			slog.Warn("image_output_transcode_failed", "output_format", format, "index", i, "error", err)
			out[i] = data
			continue
		}
		out[i] = converted
	}
	return out
}

// transcodeImage 把一张图编码为 format。原图已是目标格式时原样返回；唯一
// 例外是目标为 jpeg 且 compression < 100，此时按该质量重新压缩。
func transcodeImage(data []byte, format string, compression *int) ([]byte, error) {
	if format == "" || len(data) == 0 {
		return data, nil
	}
	if sniffImageOutputFormat(data) == format && !shouldRecompressJPEG(format, compression) {
		return data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	var buf bytes.Buffer
	switch format {
	case imageOutputFormatPNG:
		// BestSpeed：实测 2880x2880 照片约 0.8s / 18MB，默认压缩约 2.9s / 15MB。
		encoder := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := encoder.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encode png: %w", err)
		}
	case imageOutputFormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: imageJPEGQuality(compression)}); err != nil {
			return nil, fmt.Errorf("encode jpeg: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported output format %q", format)
	}
	return buf.Bytes(), nil
}

// sniffImageOutputFormat 按文件头判定 png / jpeg；其它格式返回 ""。
func sniffImageOutputFormat(data []byte) string {
	switch detectedImageContentType(data) {
	case "image/png":
		return imageOutputFormatPNG
	case "image/jpeg":
		return imageOutputFormatJPEG
	default:
		return ""
	}
}

func shouldRecompressJPEG(format string, compression *int) bool {
	return format == imageOutputFormatJPEG && compression != nil && *compression < imageJPEGDefaultQuality
}

// imageJPEGQuality 把 output_compression（0-100）换成 JPEG 质量，限制在 [1,100]；
// 未传时取 100。
func imageJPEGQuality(compression *int) int {
	if compression == nil {
		return imageJPEGDefaultQuality
	}
	return min(max(*compression, 1), imageJPEGDefaultQuality)
}
