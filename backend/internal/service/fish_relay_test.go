package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

func fishRelayAccount() *Account {
	return &Account{
		ID:       1,
		Platform: PlatformFish,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			fishCredentialRelayBaseURL: "https://api.openai.com",
			fishCredentialRelayAPIKey:  "sk-x",
		},
	}
}

func fishNativeAccount() *Account {
	return &Account{
		ID:       2,
		Platform: PlatformFish,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			fishCredentialToken: "46471c2b-7cdc-4671-acc4-c0b2fd111817",
		},
	}
}

func TestFishRelayPredicates(t *testing.T) {
	relay := fishRelayAccount()
	if !IsFishRelayAccount(relay) {
		t.Fatal("base_url account must be relay")
	}
	if isFishTokenAccount(relay) {
		t.Fatal("relay must not be token native")
	}
	native := fishNativeAccount()
	if IsFishRelayAccount(native) {
		t.Fatal("token-only account must not be relay")
	}
	if !isFishTokenAccount(native) {
		t.Fatal("token account must be native")
	}
	// 无凭据的 apikey 号两者都不是（不承接任何请求）
	bare := &Account{ID: 3, Platform: PlatformFish, Type: AccountTypeAPIKey}
	if IsFishRelayAccount(bare) || isFishTokenAccount(bare) {
		t.Fatal("bare apikey account must serve nothing")
	}
}

func TestFishAccountSupportsModel(t *testing.T) {
	relay, native := fishRelayAccount(), fishNativeAccount()
	for _, account := range []*Account{relay, native} {
		if !fishAccountSupportsModel(account, "gpt-image-2") {
			t.Fatalf("%v should support gpt-image-2", account.Type)
		}
		if fishAccountSupportsModel(account, "gpt-image-1") {
			t.Fatal("non-fish model must be rejected")
		}
		if fishAccountSupportsModel(account, "nano-banana") {
			t.Fatal("removed model must be rejected")
		}
	}
	if fishAccountSupportsModel(&Account{ID: 9}, "gpt-image-2") {
		t.Fatal("nil-ish account must not support")
	}
}

func TestFishAccountBaseURLAndAPIKeyHooks(t *testing.T) {
	relay := fishRelayAccount()
	if got := relay.GetOpenAIBaseURL(); got != "https://api.openai.com" {
		t.Fatalf("GetOpenAIBaseURL = %q", got)
	}
	if got := relay.GetOpenAIProtocolAPIKey(); got != "sk-x" {
		t.Fatalf("GetOpenAIProtocolAPIKey = %q", got)
	}
	native := fishNativeAccount()
	if got := native.GetOpenAIBaseURL(); got != "" {
		t.Fatalf("native account base_url must be empty, got %q", got)
	}
}

func TestFishRequiresRelayMatrix(t *testing.T) {
	strict := domain.FishFitLimits{MaxCropPixels: 0, MaxUpscaleFactor: 1}     // 不裁剪不放大
	generous := domain.FishFitLimits{MaxCropPixels: 8, MaxUpscaleFactor: 2.0} // 允许微裁+放大
	base := func() *OpenAIImagesRequest {
		return &OpenAIImagesRequest{Model: "gpt-image-2", Prompt: "x", N: 1, Size: "1024x1024"}
	}
	cases := []struct {
		name   string
		mut    func(*OpenAIImagesRequest)
		limits *domain.FishFitLimits
		want   bool
	}{
		{"whitelisted size", func(r *OpenAIImagesRequest) {}, nil, false},
		{"empty size defaults", func(r *OpenAIImagesRequest) { r.Size = "" }, nil, false},
		{"openai portrait size", func(r *OpenAIImagesRequest) { r.Size = "1024x1536" }, nil, true},
		{"arbitrary size", func(r *OpenAIImagesRequest) { r.Size = "123x456" }, nil, true},
		{"mask", func(r *OpenAIImagesRequest) { r.HasMask = true }, nil, true},
		{"n over cap", func(r *OpenAIImagesRequest) { r.N = 9 }, nil, true},
		{"webp", func(r *OpenAIImagesRequest) { r.OutputFormat = "webp" }, nil, true},
		{"background", func(r *OpenAIImagesRequest) { r.Background = "transparent" }, nil, true},
		{"non-fish model bad size stays false", func(r *OpenAIImagesRequest) { r.Model = "gpt-image-1"; r.Size = "123x456" }, nil, false},
		{"plannable size within limits", func(r *OpenAIImagesRequest) { r.Size = "880x1168" }, &generous, false},
		{"plannable size beyond crop limit", func(r *OpenAIImagesRequest) { r.Size = "880x1168" }, &strict, true},
		{"largest native at gpt-image pixel cap", func(r *OpenAIImagesRequest) { r.Size = "2880x2880" }, &generous, false},
		{"above gpt-image max size goes to relay", func(r *OpenAIImagesRequest) { r.Size = "5760x5760" }, &generous, true},
		{"long edge above 3840 goes to relay", func(r *OpenAIImagesRequest) { r.Size = "4096x1152" }, &generous, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mut(req)
			limits := strict
			if tc.limits != nil {
				limits = *tc.limits
			}
			if got := FishRequiresRelay(req, limits); got != tc.want {
				t.Fatalf("FishRequiresRelay = %v, want %v", got, tc.want)
			}
		})
	}
	if FishRequiresRelay(nil, strict) {
		t.Fatal("nil request must not require relay")
	}
}

func TestFishRelaySelectionExclusions(t *testing.T) {
	failed := map[int64]struct{}{1: {}}
	natives := map[int64]struct{}{2: {}, 3: {}}
	// 非 requireRelay：只有 failed
	got := FishRelaySelectionExclusions(failed, false, natives)
	if _, ok := got[2]; ok {
		t.Fatal("natives must not be excluded when relay not required")
	}
	if len(failed) != 1 {
		t.Fatal("failed must not be mutated")
	}
	// requireRelay：并入原生
	got = FishRelaySelectionExclusions(failed, true, natives)
	for _, id := range []int64{1, 2, 3} {
		if _, ok := got[id]; !ok {
			t.Fatalf("id %d missing", id)
		}
	}
}

func TestFishModelIDsMatchDomain(t *testing.T) {
	ids := FishModelIDs()
	if len(ids) != len(domain.FishModels) {
		t.Fatalf("ids = %v", ids)
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["gpt-image-2"] || found["nano-banana"] || found["seedream-5-0"] {
		t.Fatalf("ids = %v, must be gpt family only", ids)
	}
}
