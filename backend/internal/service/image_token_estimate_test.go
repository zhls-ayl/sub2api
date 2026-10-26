package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestEstimateGPTImageOutputTokens(t *testing.T) {
	cases := []struct {
		quality       string
		width, height int
		want          int
	}{
		// gpt-image-1 官方表上的尺寸：公式与官方数字一致。
		{"low", 1024, 1024, 272},
		{"medium", 1024, 1024, 1056},
		{"high", 1024, 1024, 4160},
		{"medium", 1024, 1536, 1584},
		{"HIGH", 1536, 1024, 6240},
		// 非表上尺寸按像素缩放。
		{"medium", 1072, 960, 1036},
		// xhigh / max 没有公开数据，按 high 计。
		{"xhigh", 1024, 1024, 4160},
		{"max", 1024, 1024, 4160},
		// 未知档位或非法尺寸不估算。
		{"auto", 1024, 1024, 0},
		{"medium", 0, 1024, 0},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, estimateGPTImageOutputTokens(tc.quality, tc.width, tc.height),
			"%s %dx%d", tc.quality, tc.width, tc.height)
	}
}

func TestOpenAIImageUsageJSON(t *testing.T) {
	require.Nil(t, openAIImageUsageJSON("gpt-image-2", "cat", 0), "没有输出 token 不返回 usage")

	usage := openAIImageUsageJSON("gpt-image-2", "a red cup on a table", 1056)
	textTokens := gjson.GetBytes(usage, "input_tokens").Int()
	require.Positive(t, textTokens, "提示词按 o200k 计数")
	require.Equal(t, textTokens, gjson.GetBytes(usage, "input_tokens_details.text_tokens").Int())
	require.Zero(t, gjson.GetBytes(usage, "input_tokens_details.image_tokens").Int())
	require.Equal(t, int64(1056), gjson.GetBytes(usage, "output_tokens").Int())
	require.Equal(t, int64(1056), gjson.GetBytes(usage, "output_tokens_details.image_tokens").Int())
	require.Equal(t, textTokens+1056, gjson.GetBytes(usage, "total_tokens").Int())
}
