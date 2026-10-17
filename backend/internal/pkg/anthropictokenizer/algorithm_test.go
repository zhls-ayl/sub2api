package anthropictokenizer

import "testing"

func TestParseAlgorithm(t *testing.T) {
	cases := []struct {
		in   string
		want Algorithm
	}{
		{"", AlgorithmOfficial},
		{"   ", AlgorithmOfficial},
		{"official", AlgorithmOfficial},
		{"  OFFICIAL ", AlgorithmOfficial},
		{"Official", AlgorithmOfficial},
		{"charclass", AlgorithmCharClass},
		{"CharClass", AlgorithmCharClass},
		{" weighted ", AlgorithmWeighted},
		{"WEIGHTED", AlgorithmWeighted},
		{"garbage", AlgorithmOfficial},
		{"official ", AlgorithmOfficial},
		{"charclass-ish", AlgorithmOfficial},
	}
	for _, tc := range cases {
		if got := ParseAlgorithm(tc.in); got != tc.want {
			t.Fatalf("ParseAlgorithm(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAlgorithmCountTokensDispatch(t *testing.T) {
	const sample = "The quick brown fox 你好世界 12345 !@#"
	if got, want := AlgorithmOfficial.CountTokens(sample), CountTokens(sample); got != want {
		t.Fatalf("official dispatch mismatch: %d != %d", got, want)
	}
	if got, want := AlgorithmCharClass.CountTokens(sample), CountTokensCharClass(sample); got != want {
		t.Fatalf("charclass dispatch mismatch: %d != %d", got, want)
	}
	if got, want := AlgorithmWeighted.CountTokens(sample), CountTokensWeighted(sample); got != want {
		t.Fatalf("weighted dispatch mismatch: %d != %d", got, want)
	}
}

func TestAlgorithmZeroValueDefaultsToOfficial(t *testing.T) {
	// 按值传递的结构体（如 KiroRequestContext）零值是空串，不是 "official"；
	// 分发时必须归一为官方精确口径。
	var zero Algorithm
	if string(zero) != "" {
		t.Fatalf("zero Algorithm should be empty string, got %q", zero)
	}
	const sample = "hello world"
	if got := zero.CountTokens(sample); got != CountTokens(sample) {
		t.Fatalf("zero Algorithm CountTokens = %d, want official %d", got, CountTokens(sample))
	}
}
