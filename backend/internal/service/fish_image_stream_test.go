package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 估算的 usage 是整次请求的用量：只挂在最后一个 completed 事件上，避免客户端累加重复。
func TestFishImagesStreamEventsAttachUsageToLastEvent(t *testing.T) {
	usage := fishImageUsage("gpt-image-2", "a cat", "medium", "1024x1024", 2)
	result := &FishImageResult{
		Body:    []byte(`{"created":1700000000,"data":[{"b64_json":"aaa"},{"b64_json":"bbb"}]}`),
		Size:    "1024x1024",
		Quality: "medium",
		Usage:   usage,
	}
	events := FishImagesStreamEvents(&OpenAIImagesRequest{Model: "gpt-image-2"}, result)
	require.Len(t, events, 2)
	require.False(t, gjson.GetBytes(events[0].Payload, "usage").Exists(), "非最后事件不带 usage")
	require.Equal(t, int64(2112), gjson.GetBytes(events[1].Payload, "usage.output_tokens").Int())
}
