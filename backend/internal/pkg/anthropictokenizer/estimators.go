package anthropictokenizer

import "math"

// CountTokensCharClass 按字符类别估算 token 数。
//
// 规则：将文本按 rune 分为四类，各自除以经验系数后向上取整求和——
//   - ASCII 字母与空白：4.5 字符/token
//   - ASCII 数字：2 字符/token
//   - ASCII 符号：1.5 字符/token
//   - 非 ASCII（CJK、其他文字）：1.5 字符/token
//
// 长度不足 5 个 rune 的短串按 3 字符/token 单独处理。空串返回 0。
func CountTokensCharClass(text string) int {
	if text == "" {
		return 0
	}
	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularASCII, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularASCII++
		}
	}

	estimated := int(math.Ceil(
		float64(regularASCII)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))
	if estimated < 1 {
		return 1
	}
	return estimated
}

// isWesternChar 判断字符是否属于西文书写体系（ASCII 与拉丁字母扩展区块）。
// 返回 false 表示非西文字符（如 CJK、阿拉伯文等）。
func isWesternChar(c rune) bool {
	switch {
	case c <= '\u007F': // 基本 ASCII
		return true
	case c >= '\u0080' && c <= '\u024F': // 拉丁字母扩展-A/B
		return true
	case c >= '\u1E00' && c <= '\u1EFF': // 拉丁字母扩展附加
		return true
	case c >= '\u2C60' && c <= '\u2C7F': // 拉丁字母扩展-C
		return true
	case c >= '\uA720' && c <= '\uA7FF': // 拉丁字母扩展-D
		return true
	case c >= '\uAB30' && c <= '\uAB6F': // 拉丁字母扩展-E
		return true
	default:
		return false
	}
}

// CountTokensWeighted 按字符加权和估算 token 数。
//
// 规则：
//  1. 每个西文字符计 1 个字符单位，每个非西文字符计 4 个字符单位；
//  2. token 数 = 字符单位总数 / 4；
//  3. 短文本乘以放大系数以补偿逐消息固定开销：基础 token <100 时 ×1.5、
//     <200 ×1.3、<300 ×1.25、<800 ×1.2，≥800 不放大；
//  4. 截断取整。
//
// 空串返回 0。
func CountTokensWeighted(text string) int {
	if text == "" {
		return 0
	}
	charUnits := 0.0
	for _, c := range text {
		if isWesternChar(c) {
			charUnits += 1.0
		} else {
			charUnits += 4.0
		}
	}
	tokens := charUnits / 4.0

	multiplier := 1.0
	switch {
	case tokens < 100:
		multiplier = 1.5
	case tokens < 200:
		multiplier = 1.3
	case tokens < 300:
		multiplier = 1.25
	case tokens < 800:
		multiplier = 1.2
	}
	return int(tokens * multiplier)
}
