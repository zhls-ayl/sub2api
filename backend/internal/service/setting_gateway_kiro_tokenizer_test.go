package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
)

func TestNormalizeGatewayKiroTokenizer(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", string(anthropictokenizer.AlgorithmOfficial), false},
		{"official", "official", false},
		{"  OFFICIAL ", "official", false},
		{"charclass", "charclass", false},
		{"Weighted", "weighted", false},
		{"bogus", "", true},
		{"official,weighted", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeGatewayKiroTokenizer(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("normalizeGatewayKiroTokenizer(%q) expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeGatewayKiroTokenizer(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeGatewayKiroTokenizer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
