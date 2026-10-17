package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	leonardo "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
)

// Leonardo Cookie 号的出图上限（账号 extra）。超过上限时按上限出图，尺寸再在网关
// 本地等比超分回客户请求的尺寸；计费按交付尺寸。见 leonardo.PlanImage。
const (
	leonardoMaxQualityExtraKey       = "leonardo_max_quality"
	leonardoMaxShortEdgeExtraKey     = "leonardo_max_short_edge"
	leonardoMaxUpscaleFactorExtraKey = "leonardo_max_upscale_factor"

	leonardoMinShortEdgeLimit = 640  // 上游高度清单的最小值
	leonardoMaxShortEdgeLimit = 3584 // 上游高度清单的最大值
	leonardoMinUpscaleFactor  = 1.0
	leonardoMaxUpscaleFactor  = 4.0
)

// LeonardoImageLimits 返回账号的出图上限；非 Leonardo Cookie 号或未配置时为零值（不限）。
// 读取宽松（无效值按不限处理），写入时由 ValidateLeonardoImageLimitsFromExtra 把关。
func (a *Account) LeonardoImageLimits() leonardo.ImageLimits {
	var limits leonardo.ImageLimits
	if !isLeonardoCookieAccount(a) || a.Extra == nil {
		return limits
	}
	if quality, _ := a.Extra[leonardoMaxQualityExtraKey].(string); leonardo.IsKnownQuality(quality) {
		limits.MaxQuality = strings.ToUpper(strings.TrimSpace(quality))
	}
	if edge := parseExtraInt(a.Extra[leonardoMaxShortEdgeExtraKey]); edge >= leonardoMinShortEdgeLimit && edge <= leonardoMaxShortEdgeLimit {
		limits.MaxShortEdge = edge
	}
	if factor := parseExtraFloat64(a.Extra[leonardoMaxUpscaleFactorExtraKey]); factor >= leonardoMinUpscaleFactor && factor <= leonardoMaxUpscaleFactor {
		limits.MaxUpscaleFactor = factor
	}
	return limits
}

// ValidateLeonardoImageLimitsFromExtra 校验并就地归一化 Leonardo 出图上限：
//   - leonardo_max_quality：空 = 不限，否则 low / medium / high / xhigh / max（存小写）；
//   - leonardo_max_short_edge：0 = 不限，否则 640..3584 的整数像素；
//   - leonardo_max_upscale_factor：0 = 默认（2），否则 1..4。
//
// 缺失或 nil 的 key 视为不限。
func ValidateLeonardoImageLimitsFromExtra(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	if raw, ok := extra[leonardoMaxQualityExtraKey]; ok && raw != nil {
		quality, isString := raw.(string)
		quality = strings.ToLower(strings.TrimSpace(quality))
		if !isString || (quality != "" && !leonardo.IsKnownQuality(quality)) {
			return fmt.Errorf("%s must be one of low, medium, high, xhigh or max", leonardoMaxQualityExtraKey)
		}
		extra[leonardoMaxQualityExtraKey] = quality
	}
	if raw, ok := extra[leonardoMaxShortEdgeExtraKey]; ok && raw != nil {
		value, err := strictExtraNumber(raw)
		if err != nil || value != math.Trunc(value) ||
			(value != 0 && (value < leonardoMinShortEdgeLimit || value > leonardoMaxShortEdgeLimit)) {
			return fmt.Errorf("%s must be 0 or an integer between %d and %d",
				leonardoMaxShortEdgeExtraKey, leonardoMinShortEdgeLimit, leonardoMaxShortEdgeLimit)
		}
		extra[leonardoMaxShortEdgeExtraKey] = int(value)
	}
	if raw, ok := extra[leonardoMaxUpscaleFactorExtraKey]; ok && raw != nil {
		value, err := strictExtraNumber(raw)
		if err != nil || (value != 0 && (value < leonardoMinUpscaleFactor || value > leonardoMaxUpscaleFactor)) {
			return fmt.Errorf("%s must be 0 or between %g and %g",
				leonardoMaxUpscaleFactorExtraKey, leonardoMinUpscaleFactor, leonardoMaxUpscaleFactor)
		}
		extra[leonardoMaxUpscaleFactorExtraKey] = value
	}
	return nil
}

// strictExtraNumber 把 extra 里的数值（数字或数字字符串）解析为有限的 float64；
// 空字符串按 0 处理，其余无法解析的值返回错误。
func strictExtraNumber(raw any) (float64, error) {
	var value float64
	switch v := raw.(type) {
	case float64:
		value = v
	case float32:
		value = float64(v)
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, err
		}
		value = parsed
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, nil
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, err
		}
		value = parsed
	default:
		return 0, fmt.Errorf("not a number")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("not a finite number")
	}
	return value, nil
}
