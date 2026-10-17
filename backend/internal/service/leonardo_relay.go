package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
)

// IsLeonardoRelayAccount 判断账号是否为 Leonardo 组内的 OpenAI 形中转号：
// platform=leonardo、type=apikey、且配置了非空 base_url。
//
// 这类账号不打 Leonardo GraphQL，而是把 /v1/images/generations|edits 转发到
// {base_url}（官方 OpenAI 或三方兼容端点），作为原生 Cookie 号全部故障时的
// 兜底。没有 base_url 的 leonardo apikey 不是中转号——本渠道不支持 Leonardo
// 官方 Platform API Key。
func IsLeonardoRelayAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformLeonardo || account.Type != AccountTypeAPIKey {
		return false
	}
	return strings.TrimSpace(account.GetCredential("base_url")) != ""
}

func isLeonardoRelayAccount(account *Account) bool {
	return IsLeonardoRelayAccount(account)
}

// isLeonardoCookieAccount 报告账号是否为 Leonardo Cookie 原生号（platform=leonardo、
// type=oauth）：只有它直接消耗 Leonardo 积分。
func isLeonardoCookieAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformLeonardo && account.Type == AccountTypeOAuth
}

// shouldSkipLeonardoNativeAccount 在本次请求已撞上内容安全拒绝后，禁止再打
// Cookie 原生号（同样的输入大概率再次被拒）。中转号不受影响。
func shouldSkipLeonardoNativeAccount(skipNative bool, account *Account) bool {
	return skipNative && !isLeonardoRelayAccount(account)
}

// leonardoAccountSupportsModel 判定 Leonardo 账号能否承接 requestedModel：
//   - 只有 Cookie（oauth）号与带 base_url 的中转号有可用上游，其余（如缺
//     base_url 的 apikey 号）一律不承接；
//   - 映射后的模型必须是平台已实现的模型，自定义映射也绕不过去。Cookie 号
//     接受对外名或上游 slug（出图时统一换成 slug）；中转号只接受对外名——
//     openai/… 这类 slug 转发给 OpenAI 兼容上游对方不认。
func leonardoAccountSupportsModel(account *Account, requestedModel string) bool {
	if account == nil {
		return false
	}
	upstreamModel, matched := account.ResolveMappedModel(requestedModel)
	if !matched {
		return false
	}
	switch {
	case isLeonardoRelayAccount(account):
		return domain.IsLeonardoPublicModel(upstreamModel)
	case account.Type == AccountTypeOAuth:
		_, ok := domain.LeonardoNativeSlug(upstreamModel)
		return ok
	default:
		return false
	}
}

// LeonardoModelIDs 返回 Leonardo 平台的对外模型名（展示顺序）。
func LeonardoModelIDs() []string {
	return domain.LeonardoModelNames()
}

// LeonardoRequiresRelay 判断哪些 OpenAI Images 参数组合原生路径不支持、
// 只能由中转号服务：mask 局部重绘（Leonardo GraphQL 无 inpaint 语义）、超过
// 原生张数上限（quantity ≤ 8）与 output_format=webp（本机只转码 png / jpeg）。
// 透明背景、n≤8、png / jpeg 原生支持；stream=true 在原生号上走假流式（handler
// 的 leonardoFakeStream）。命中且无可用中转号时返回明确 400，不降级为原生
// 出图（降级会产生错误结果）。
func LeonardoRequiresRelay(req *OpenAIImagesRequest) bool {
	if req == nil {
		return false
	}
	return req.HasMask || req.N > leonardopkg.MaxImageQuantity || isImageOutputFormatWebP(req.OutputFormat)
}

// LeonardoRelaySelectionExclusions 构造「只走中转」阶段的选号排除集。
//
// failed 只拷贝，不就地改写，这样中转耗尽后原生号仍能被选到。
// requireRelay 时才并入 extraNativeIDs（选号循环里漏网的原生号）。
func LeonardoRelaySelectionExclusions(failed map[int64]struct{}, requireRelay bool, extraNativeIDs map[int64]struct{}) map[int64]struct{} {
	out := make(map[int64]struct{}, len(failed)+len(extraNativeIDs))
	for id := range failed {
		out[id] = struct{}{}
	}
	if requireRelay {
		for id := range extraNativeIDs {
			out[id] = struct{}{}
		}
	}
	return out
}

// ExcludeLeonardoNativeAccounts 把本组可调度的非中转 Leonardo 账号并入
// excluded，避免内容拒绝或「仅中转特性」阶段把换号预算浪费在不会成功的
// Cookie 号上。
func (s *GatewayService) ExcludeLeonardoNativeAccounts(ctx context.Context, groupID *int64, excluded map[int64]struct{}) {
	if s == nil || excluded == nil {
		return
	}
	accounts, _, err := s.listSchedulableAccounts(ctx, groupID, PlatformLeonardo, true)
	if err != nil {
		return
	}
	for i := range accounts {
		if !isLeonardoRelayAccount(&accounts[i]) {
			excluded[accounts[i].ID] = struct{}{}
		}
	}
}
