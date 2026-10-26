package service

import (
	"fmt"
	"strings"
)

// fishMaxQualityExtraKey 是 Fish 原生号的最大出图质量（账号 extra）。
// 空 = 不限。请求高于上限时按上限向上游出图，响应仍回显客户请求的档位。
const fishMaxQualityExtraKey = "fish_max_quality"

// fishQualityRank 是 Fish 质量档的高低顺序。max 是最高档。
// xhigh / max 只有 gpt-image-2.5 系列会真正发到上游。
var fishQualityRank = map[string]int{
	"low":    1,
	"medium": 2,
	"high":   3,
	"xhigh":  4,
	"max":    5,
}

// FishMaxQuality 返回原生号的最大质量（小写）；中转号、未配置或无效值视为不限。
// 读取宽松，写入时由 ValidateFishMaxQualityFromExtra 把关。
func (a *Account) FishMaxQuality() string {
	if !isFishTokenAccount(a) || a.Extra == nil {
		return ""
	}
	quality, _ := a.Extra[fishMaxQualityExtraKey].(string)
	quality = strings.ToLower(strings.TrimSpace(quality))
	if _, ok := fishQualityRank[quality]; !ok {
		return ""
	}
	return quality
}

// ValidateFishMaxQualityFromExtra 校验并就地归一化 fish_max_quality：
// 空 = 不限，否则 low / medium / high / xhigh / max（存小写，max 最高）。
// 缺失或 nil 视为不限。
func ValidateFishMaxQualityFromExtra(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	raw, ok := extra[fishMaxQualityExtraKey]
	if !ok || raw == nil {
		return nil
	}
	quality, isString := raw.(string)
	quality = strings.ToLower(strings.TrimSpace(quality))
	if !isString || (quality != "" && fishQualityRank[quality] == 0) {
		return fmt.Errorf("%s must be empty or one of low, medium, high, xhigh, max", fishMaxQualityExtraKey)
	}
	extra[fishMaxQualityExtraKey] = quality
	return nil
}

// fishCapQuality 返回 requested 与上限 max 中较低的一档；max 为空或未知时原样返回。
func fishCapQuality(requested, max string) string {
	maxRank, ok := fishQualityRank[strings.ToLower(strings.TrimSpace(max))]
	if !ok {
		return requested
	}
	if rank, ok := fishQualityRank[requested]; ok && rank > maxRank {
		return strings.ToLower(strings.TrimSpace(max))
	}
	return requested
}
