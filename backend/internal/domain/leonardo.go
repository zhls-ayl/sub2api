package domain

import "strings"

// LeonardoModel 是 Leonardo 平台的一个对外模型：Name 是客户端请求与计费用的
// 对外名（API Key 中转号原样转发它），NativeSlug 是 Cookie 原生号 Generate
// 请求里的上游 model 字段。
type LeonardoModel struct {
	Name       string
	NativeSlug string
	// ExtendedQuality 为 true 时额外支持 XHIGH / MAX 质量档（gpt-image-2.5 系列，
	// 2026-10 抓包确认）；gpt-image-2 只有 LOW / MEDIUM / HIGH。
	ExtendedQuality bool
}

// LeonardoModels 是 Leonardo 平台已实现的模型（展示顺序）。gpt-image-2.5 系列
// 与 gpt-image-2 共用宽高允许值清单（2026-10 实测），质量档额外多 XHIGH / MAX（抓包确认），上游
// slug 带 openai/ 前缀。
var LeonardoModels = []LeonardoModel{
	{Name: "gpt-image-2", NativeSlug: "gpt-image-2"},
	{Name: "gpt-image-2.5-flare", NativeSlug: "openai/gpt-image-2.5-flare", ExtendedQuality: true},
	{Name: "gpt-image-2.5-sunburst", NativeSlug: "openai/gpt-image-2.5-sunburst", ExtendedQuality: true},
}

// DefaultLeonardoModelMapping 是 Leonardo 账号（Cookie 原生号与 API Key 中转号
// 共用）的默认模型映射：对外名恒等映射。原生号在出图时再经 LeonardoNativeSlug
// 换成上游 slug，所以白名单模式生成的恒等映射对两类账号都成立。
var DefaultLeonardoModelMapping = func() map[string]string {
	mapping := make(map[string]string, len(LeonardoModels))
	for _, model := range LeonardoModels {
		mapping[model.Name] = model.Name
	}
	return mapping
}()

// lookupLeonardoModel 按对外名或上游 slug 查模型（大小写不敏感）。
func lookupLeonardoModel(model string) (LeonardoModel, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return LeonardoModel{}, false
	}
	for _, candidate := range LeonardoModels {
		if strings.EqualFold(candidate.Name, model) || strings.EqualFold(candidate.NativeSlug, model) {
			return candidate, true
		}
	}
	return LeonardoModel{}, false
}

// LeonardoNativeSlug 把对外名或上游 slug 解析为 Cookie 原生号使用的上游 slug
// （大小写不敏感）。未知模型返回 ok=false。
func LeonardoNativeSlug(model string) (string, bool) {
	found, ok := lookupLeonardoModel(model)
	return found.NativeSlug, ok
}

// LeonardoSupportsExtendedQuality 报告模型（对外名或上游 slug）是否支持
// XHIGH / MAX 质量档。
func LeonardoSupportsExtendedQuality(model string) bool {
	found, ok := lookupLeonardoModel(model)
	return ok && found.ExtendedQuality
}

// IsLeonardoPublicModel 报告 model 是否为 Leonardo 平台的对外名（大小写不敏感）。
// 中转号只能转发对外名：上游 slug（openai/…）OpenAI 兼容端点不认。
func IsLeonardoPublicModel(model string) bool {
	model = strings.TrimSpace(model)
	for _, candidate := range LeonardoModels {
		if strings.EqualFold(candidate.Name, model) {
			return true
		}
	}
	return false
}

// LeonardoModelNames 返回对外名列表（展示顺序）。
func LeonardoModelNames() []string {
	names := make([]string, 0, len(LeonardoModels))
	for _, model := range LeonardoModels {
		names = append(names, model.Name)
	}
	return names
}
