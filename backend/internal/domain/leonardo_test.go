package domain

import "testing"

func TestLeonardoModelSlugs(t *testing.T) {
	cases := map[string]string{
		"gpt-image-2":                   "gpt-image-2",
		"GPT-Image-2.5-Flare":           "openai/gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst":        "openai/gpt-image-2.5-sunburst",
		"openai/gpt-image-2.5-sunburst": "openai/gpt-image-2.5-sunburst",
	}
	for model, want := range cases {
		if got, ok := LeonardoNativeSlug(model); !ok || got != want {
			t.Fatalf("LeonardoNativeSlug(%q) = %q, %v; want %q", model, got, ok, want)
		}
	}
	for _, unknown := range []string{"", "gpt-image-1", "openai/gpt-image-9"} {
		if _, ok := LeonardoNativeSlug(unknown); ok {
			t.Fatalf("LeonardoNativeSlug(%q) should be unknown", unknown)
		}
	}

	if !IsLeonardoPublicModel("gpt-image-2.5-flare") || IsLeonardoPublicModel("openai/gpt-image-2.5-flare") {
		t.Fatalf("public model check must accept public names only")
	}
	if len(DefaultLeonardoModelMapping) != 3 || DefaultLeonardoModelMapping["gpt-image-2.5-sunburst"] != "gpt-image-2.5-sunburst" {
		t.Fatalf("default mapping must be identity over public names: %v", DefaultLeonardoModelMapping)
	}
}

func TestLeonardoSupportsExtendedQuality(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5-flare", "openai/gpt-image-2.5-sunburst", "GPT-IMAGE-2.5-SUNBURST"} {
		if !LeonardoSupportsExtendedQuality(model) {
			t.Fatalf("%s should support XHIGH / MAX", model)
		}
	}
	for _, model := range []string{"gpt-image-2", "gpt-image-9", ""} {
		if LeonardoSupportsExtendedQuality(model) {
			t.Fatalf("%s should not support XHIGH / MAX", model)
		}
	}
}
