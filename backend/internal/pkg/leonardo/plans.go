package leonardo

import "strings"

// PlanQuota 是个人套餐的积分额度与能力。
//
// 上游 user_details 只返回各积分池的剩余量，不返回上限；个人套餐的上限连
// Leonardo 网页版自己也是前端写死的常量（PLAN_TOKENS / PLAN_MAX_ROLLOVER_TOKENS /
// PLAN_NAME_V2，抓自 app.leonardo.ai 前端包，2026-10）。team_plans 只覆盖团队
// 套餐且与个人套餐同名冲突（团队 PRO=100k、个人 PRO=60k），不能拿来查个人号。
// Leonardo 调整套餐额度时需要同步这里。
type PlanQuota struct {
	// FastTokens 是每个周期发放的 Fast Tokens（subscriptionTokens 的上限）。
	FastTokens int
	// MaxRolloverTokens 是 Rollover Bank 的容量（rolloverTokens 的上限）。
	MaxRolloverTokens int
	// DisplayName 是网页版展示的套餐名（STANDARD → Premium）。
	DisplayName string
	// DailyFast 为 true 时 Fast Tokens 按天发放（FREE 套餐）。
	DailyFast bool
	// CanUseRollover / CanUseTopUp 对应网页版的 canUseRolloverTokens /
	// canUseTopUpTokens：免费套餐的结转积分不可用，Top-up 需要付费订阅。
	CanUseRollover bool
	CanUseTopUp    bool
}

var planQuotas = map[string]PlanQuota{
	"FREE":     {FastTokens: 150, MaxRolloverTokens: 0, DisplayName: "Free", DailyFast: true},
	"FREEPLUS": {FastTokens: 8000, MaxRolloverTokens: 0, DisplayName: "Free Plus", CanUseTopUp: true},
	"BASIC":    {FastTokens: 8500, MaxRolloverTokens: 25500, DisplayName: "Essential", CanUseRollover: true, CanUseTopUp: true},
	"STANDARD": {FastTokens: 25000, MaxRolloverTokens: 75000, DisplayName: "Premium", CanUseRollover: true, CanUseTopUp: true},
	"PRO":      {FastTokens: 60000, MaxRolloverTokens: 180000, DisplayName: "Ultimate", CanUseRollover: true, CanUseTopUp: true},
}

// LookupPlan 按 user_details.plan 查个人套餐额度（大小写不敏感）。未知套餐返回
// ok=false，调用方应退回「只展示数字、不推断上限与可用性」。
func LookupPlan(plan string) (PlanQuota, bool) {
	quota, ok := planQuotas[strings.ToUpper(strings.TrimSpace(plan))]
	return quota, ok
}
