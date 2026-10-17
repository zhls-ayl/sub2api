package leonardo

import "strings"

// DefaultMaxUpscaleFactor 是未配置最大放大倍数时的默认值：插值放大不会补出
// 细节，超过 2 倍时画面明显发软。
const DefaultMaxUpscaleFactor = 2.0

// ImageLimits 是账号级的出图上限（零值 = 不限）。超过上限时按上限出图，
// 尺寸再在网关本地等比超分回客户请求的尺寸（见 PlanImage）。
type ImageLimits struct {
	// MaxQuality：LOW / MEDIUM / HIGH / XHIGH / MAX；空 = 不限。
	MaxQuality string
	// MaxShortEdge 是出图短边上限（像素）；≤0 = 不限。
	MaxShortEdge int
	// MaxUpscaleFactor 是本地超分的最大倍数；≤0 按 DefaultMaxUpscaleFactor。
	MaxUpscaleFactor float64
}

// ImagePlan 是一次出图的尺寸与质量决策。
type ImagePlan struct {
	// Target 是交付给客户的尺寸（客户请求的 size）。
	Target ImageSlot
	// Source 是实际向上游请求的尺寸；与 Target 不同时出图后本地等比超分到 Target。
	Source ImageSlot
	// RequestedQuality 是归一化后的客户请求档位（响应回显用）。
	RequestedQuality string
	// Quality 是实际出图档位（不超过 ImageLimits.MaxQuality）。
	Quality string
}

// Upscaled 报告出图后是否需要本地超分。
func (p *ImagePlan) Upscaled() bool {
	return p != nil && p.Source != p.Target
}

// qualityRank 是质量档的高低顺序。
var qualityRank = map[string]int{
	QualityLow:    1,
	QualityMedium: 2,
	QualityHigh:   3,
	QualityXHigh:  4,
	QualityMax:    5,
}

// IsKnownQuality 报告 quality（大小写不敏感）是否为已知质量档。
func IsKnownQuality(quality string) bool {
	_, ok := qualityRank[strings.ToUpper(strings.TrimSpace(quality))]
	return ok
}

// CapQuality 返回 quality 与上限 max 中较低的一档；max 为空或未知时原样返回。
func CapQuality(quality, max string) string {
	maxRank, ok := qualityRank[strings.ToUpper(strings.TrimSpace(max))]
	if !ok {
		return quality
	}
	if rank, ok := qualityRank[quality]; ok && rank > maxRank {
		return strings.ToUpper(strings.TrimSpace(max))
	}
	return quality
}

// upstreamAllowedWidths / upstreamAllowedHeights 是 Leonardo Generate 对 gpt-image 系列
// 宽、高各自的允许值（2026-10 实测 VALIDATION_ERROR 报错原文，宽高可自由组合）。
// 透传客户尺寸时仍由上游校验；这里只在超分降尺寸时用来挑一个上游能接受的出图
// 尺寸——上游没有提供查询接口，只能写死。上游改了清单时，挑出的尺寸会被
// VALIDATION_ERROR 拒绝（不扣积分，报错带新清单回给客户端并进日志），照着更新即可。
var (
	upstreamAllowedWidths = []int{
		768, 832, 848, 864, 896, 928, 1024, 1088, 1136, 1152, 1184, 1200, 1248, 1264, 1344, 1376, 1536,
		1584, 1648, 1696, 1792, 1856, 2016, 2048, 2336, 2448, 2560, 2880, 3200, 3264, 3504, 3584, 3808,
	}
	upstreamAllowedHeights = []int{
		640, 672, 768, 832, 848, 864, 896, 928, 1024, 1136, 1152, 1184, 1200, 1248, 1264, 1280, 1344, 1376, 1536,
		1584, 1632, 1648, 1696, 1792, 1824, 1856, 1920, 2016, 2048, 2336, 2448, 2560, 2880, 3200, 3264, 3504, 3584,
	}
)

// PlanImage 归一化质量与尺寸，并按账号上限决定实际出图的质量与尺寸。
//
// 质量：超过 MaxQuality 时按上限出图。尺寸：客户尺寸短边超过 MaxShortEdge 时，
// 在上游允许的宽高组合里挑比例精确相等（w*H == h*W）、放大倍数不超过
// MaxUpscaleFactor 的更小尺寸出图，出图后本地等比超分回客户尺寸——不拉伸、不裁剪。
// 优先选短边不超过上限的候选中最大的；没有时选候选中最小的（仍比原尺寸省）；
// 一个候选都没有（如 2048x1136 这类本身不是精确比例的尺寸）就按客户尺寸出图。
func PlanImage(opts ImageOptions) (*ImagePlan, error) {
	requested, err := NormalizeQuality(opts.Quality, opts.ExtendedQuality)
	if err != nil {
		return nil, err
	}
	target, err := ResolveImageSlot(opts.Size)
	if err != nil {
		return nil, err
	}
	plan := &ImagePlan{
		Target:           *target,
		Source:           *target,
		RequestedQuality: requested,
		Quality:          CapQuality(requested, opts.Limits.MaxQuality),
	}
	if source, ok := upscaleSource(*target, opts.Limits); ok {
		plan.Source = source
	}
	return plan, nil
}

// upscaleSource 为 target 挑一个比例精确相等的更小出图尺寸；不需要或挑不到时 ok=false。
func upscaleSource(target ImageSlot, limits ImageLimits) (ImageSlot, bool) {
	maxShort := limits.MaxShortEdge
	if maxShort <= 0 || min(target.Width, target.Height) <= maxShort {
		return ImageSlot{}, false
	}
	maxFactor := limits.MaxUpscaleFactor
	if maxFactor <= 0 {
		maxFactor = DefaultMaxUpscaleFactor
	}

	var best, smallest ImageSlot
	for _, w := range upstreamAllowedWidths {
		if w >= target.Width || float64(target.Width) > maxFactor*float64(w)+1e-9 {
			continue
		}
		for _, h := range upstreamAllowedHeights {
			if w*target.Height != h*target.Width || checkImageDimensions(w, h) != nil {
				continue
			}
			pixels := w * h
			if min(w, h) <= maxShort {
				if pixels > best.Width*best.Height {
					best = ImageSlot{Width: w, Height: h}
				}
			} else if smallest.Width == 0 || pixels < smallest.Width*smallest.Height {
				smallest = ImageSlot{Width: w, Height: h}
			}
		}
	}
	switch {
	case best.Width > 0:
		return best, true
	case smallest.Width > 0:
		return smallest, true
	default:
		return ImageSlot{}, false
	}
}
