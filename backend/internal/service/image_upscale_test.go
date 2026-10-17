package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func decodedSize(t *testing.T, data []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	return cfg.Width, cfg.Height, format
}

func TestUpscaleImageKeepsFormatAndExactSize(t *testing.T) {
	out, err := upscaleImage(encodeTestJPEG(t, 96, 54, 90), 160, 90, "", nil)
	require.NoError(t, err)
	w, h, format := decodedSize(t, out)
	require.Equal(t, 160, w)
	require.Equal(t, 90, h)
	require.Equal(t, "jpeg", format, "未指定 output_format 时保持原图格式")

	out, err = upscaleImage(encodeTestJPEG(t, 96, 54, 90), 192, 108, imageOutputFormatPNG, nil)
	require.NoError(t, err)
	w, h, format = decodedSize(t, out)
	require.Equal(t, []any{192, 108, "png"}, []any{w, h, format})
}

func TestUpscaleImageRejectsAspectRatioChange(t *testing.T) {
	_, err := upscaleImage(encodeTestJPEG(t, 96, 54, 90), 200, 100, "", nil)
	require.ErrorContains(t, err, "aspect ratio mismatch", "比例不一致时绝不拉伸")
}

func TestUpscaleImagePreservesTransparency(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			src.Set(x, y, color.NRGBA{R: 200, G: 50, B: 50, A: uint8(x * 8)})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, src))

	out, err := upscaleImage(buf.Bytes(), 64, 64, "", nil)
	require.NoError(t, err)
	img, err := png.Decode(bytes.NewReader(out))
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, 64, 64), img.Bounds())
	_, _, _, leftAlpha := img.At(0, 32).RGBA()
	_, _, _, rightAlpha := img.At(63, 32).RGBA()
	require.Less(t, leftAlpha, uint32(0x2000), "左侧应接近全透明")
	require.Greater(t, rightAlpha, uint32(0xC000), "右侧应接近不透明")
}

func TestUpscaleImagesFallsBackToOriginalSize(t *testing.T) {
	broken := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 32)...)
	good := encodeTestJPEG(t, 48, 32, 90)

	out := upscaleImages([][]byte{broken, good}, 96, 64, "", nil)
	require.Equal(t, broken, out[0], "无法解码时原样保留")
	w, h, _ := decodedSize(t, out[1])
	require.Equal(t, []int{96, 64}, []int{w, h})

	// 超分失败（比例不符）时退回原尺寸，但仍按 output_format 转码。
	out = upscaleImages([][]byte{good}, 100, 100, imageOutputFormatPNG, nil)
	w, h, format := decodedSize(t, out[0])
	require.Equal(t, []any{48, 32, "png"}, []any{w, h, format})
}
