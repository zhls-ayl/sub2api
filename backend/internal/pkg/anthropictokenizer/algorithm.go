package anthropictokenizer

import "strings"

// Algorithm 是 token 计数算法的选择值。零值与未知值均归一为
// AlgorithmOfficial，因此 KiroRequestContext 等按值传递的结构体无需
// 显式初始化即默认官方精确口径。
type Algorithm string

const (
	// AlgorithmOfficial 使用本包移植的 Anthropic 参考 BPE 精确计数。
	AlgorithmOfficial Algorithm = "official"
	// AlgorithmCharClass 使用 CountTokensCharClass 字符分类启发式估算。
	AlgorithmCharClass Algorithm = "charclass"
	// AlgorithmWeighted 使用 CountTokensWeighted 字符加权启发式估算。
	AlgorithmWeighted Algorithm = "weighted"
)

// ParseAlgorithm 将存储或配置层传来的字符串归一为 Algorithm。
// 空串、空白与未知值返回 AlgorithmOfficial。
func ParseAlgorithm(s string) Algorithm {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(AlgorithmCharClass):
		return AlgorithmCharClass
	case string(AlgorithmWeighted):
		return AlgorithmWeighted
	default:
		return AlgorithmOfficial
	}
}

// CountTokens 按算法分发计数。零值算法等价于 AlgorithmOfficial。
func (a Algorithm) CountTokens(text string) int {
	switch ParseAlgorithm(string(a)) {
	case AlgorithmCharClass:
		return CountTokensCharClass(text)
	case AlgorithmWeighted:
		return CountTokensWeighted(text)
	default:
		return CountTokens(text)
	}
}
