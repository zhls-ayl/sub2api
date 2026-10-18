package anthropictokenizer

import (
	"strings"
	"testing"
)

func TestCountTokensCharClass(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"short ascii", "abc", 1},
		{"short ascii 4", "abcd", 2},
		{"ascii word", "hello", 2},
		{"short cjk", "你好", 1},
		{"cjk sentence", "你好世界测试文本", 6}, // 8 个非 ASCII 字符，ceil(8/1.5)=6
		{"digits", "123456", 3},
		{"symbols", "!!!!!!", 4},
		{"ascii prose", "The quick brown fox jumps over the lazy dog.", 11}, // 43 regular + 1 symbol
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountTokensCharClass(tc.text); got != tc.want {
				t.Fatalf("CountTokensCharClass(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestCountTokensCharClassInflatesRelativeOfficialOnStructuredText(t *testing.T) {
	// JSON 工具 schema 与 CJK 是启发式相对 BPE 最失真的两类内容。
	samples := []string{
		`{"name":"read_file","description":"Read the contents of a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`,
		"这是一段中文文本，用来验证启发式估算相对参考分词器的膨胀方向。",
	}
	for _, s := range samples {
		if CountTokensCharClass(s) <= CountTokens(s) {
			t.Fatalf("charclass should inflate over official for %q: got %d <= %d",
				s[:min(30, len(s))], CountTokensCharClass(s), CountTokens(s))
		}
	}
}

func TestCountTokensWeighted(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"single ascii word", "hello", 1},
		{"two ascii words", "hello world", 4},
		{"cjk sentence", "你好世界", 6},
		// 分档边界：tier 阈值作用于基础 token（units/4）。
		{"tier boundary just below 100", strings.Repeat("a", 399), 149},  // 99.75*1.5
		{"tier boundary at 100", strings.Repeat("a", 400), 130},          // 100*1.3
		{"tier boundary just below 200", strings.Repeat("a", 796), 258},  // 199*1.3
		{"tier boundary at 200", strings.Repeat("a", 800), 250},          // 200*1.25
		{"tier boundary just below 300", strings.Repeat("a", 1196), 373}, // 299 落在 [200,300) 档 ×1.25
		{"tier boundary at 300", strings.Repeat("a", 1200), 360},         // 300*1.2
		{"tier boundary just below 800", strings.Repeat("a", 3196), 958}, // 799*1.2
		{"tier boundary at 800", strings.Repeat("a", 3200), 800},         // 800*1.0
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountTokensWeighted(tc.text); got != tc.want {
				t.Fatalf("CountTokensWeighted(len=%d) = %d, want %d", len(tc.text), got, tc.want)
			}
		})
	}
}

func TestCountTokensWeightedInflatesRelativeOfficialOnCJK(t *testing.T) {
	cjk := "大模型会把每个汉字切成一个或多个 token，中文内容在加权口径下显著膨胀。"
	if CountTokensWeighted(cjk) <= CountTokens(cjk) {
		t.Fatalf("weighted should inflate over official on CJK: got %d <= %d",
			CountTokensWeighted(cjk), CountTokens(cjk))
	}
}

func TestCountTokensWeightedMonotonicWithinTier(t *testing.T) {
	// 同一分档内文本变长，计数不减。跨档位边界（×1.5→×1.3 等）按设计向下跳变，
	// 不在断言范围内：仅覆盖 tier 1（基础 token <100，即长度 <400 的纯 ASCII）。
	prev := 0
	for n := 1; n < 400; n += 3 {
		got := CountTokensWeighted(strings.Repeat("a", n))
		if got < prev {
			t.Fatalf("count decreased at n=%d: %d < %d", n, got, prev)
		}
		prev = got
	}
}
