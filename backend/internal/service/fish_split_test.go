package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/fish"
)

// fishSplitUpstream 模拟按 count 出图的假上游：每个任务组返回与提交
// count 相同数量的产物；failOnSubmit>=1 时第 N 次提交返回 402。
func fishSplitUpstream(t *testing.T, submitCount *int32, failOnSubmit int32) http.Handler {
	mux := http.NewServeMux()
	groupCounts := map[string]int{}
	mux.HandleFunc("/tools/image-video/price", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"credits":300,"perItemCredits":150,"pricingVersion":"pv"}`))
	})
	mux.HandleFunc("/tools/image-video/generate", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(submitCount, 1)
		var body struct {
			Count int `json:"count"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if failOnSubmit >= 1 && n == failOnSubmit {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"status":402,"code":"OUT_OF_CREDITS","message":"Out of credits"}`))
			return
		}
		gid := "grp" + string(rune('0'+n))
		groupCounts[gid] = body.Count
		results := make([]map[string]any, body.Count)
		for i := range results {
			results[i] = map[string]any{"id": "m", "status": "generating"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": gid, "status": "generating", "results": results,
		})
	})
	mux.HandleFunc("/task/group/", func(w http.ResponseWriter, r *http.Request) {
		gid := r.URL.Path[len("/task/group/"):]
		count := groupCounts[gid]
		if count == 0 {
			count = 1
		}
		results := make([]map[string]any, count)
		for i := range results {
			results[i] = map[string]any{
				"id": "m", "status": "done",
				"mediaItem": map[string]any{"id": "m", "url": "http://" + r.Host + "/img.png"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": gid, "status": "done", "results": results})
	})
	mux.HandleFunc("/img.png", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("imgdata"))
	})
	return mux
}

func fishAccountWithCap(cap int) *Account {
	creds := map[string]any{
		fishCredentialToken:    "tok",
		fishCredentialTeamID:   "t",
		fishCredentialDeviceID: "d",
	}
	if cap > 0 {
		creds[fishCredentialMaxPerGen] = cap
	}
	return &Account{ID: 1, Type: AccountTypeAPIKey, Credentials: creds}
}

func TestFishGenerateCallSplitsByAccountCap(t *testing.T) {
	var submits int32
	service, _, _ := newFishTestService(t, fishSplitUpstream(t, &submits, 0))
	account2 := fishAccountWithCap(2)
	// n=4, cap=2 → 2 次提交
	call := NewFishImageCall(fishImageRequest("gpt-image-2", "", "", 4), "gpt-image-2", domain.FishFitLimits{})
	result, err := service.GenerateCall(context.Background(), account2, "tok", call)
	if err != nil {
		t.Fatalf("GenerateCall: %v", err)
	}
	if submits != 2 {
		t.Fatalf("submits = %d, want 2 (split by cap)", submits)
	}
	if result.Forward.ImageCount != 4 {
		t.Fatalf("ImageCount = %d, want 4", result.Forward.ImageCount)
	}
	if call.Remaining() != 0 || call.Delivered() != 4 {
		t.Fatalf("call state: remaining=%d delivered=%d", call.Remaining(), call.Delivered())
	}
}

func TestFishGenerateCallPartialProgressOnQuota(t *testing.T) {
	var submits int32
	service, _, _ := newFishTestService(t, fishSplitUpstream(t, &submits, 3))
	account := fishAccountWithCap(2)
	// n=6, cap=2 → 第 1、2 组成功（4 张交付），第 3 组 402 → 可换号错误 + 进度保留
	call := NewFishImageCall(fishImageRequest("gpt-image-2", "", "", 6), "gpt-image-2", domain.FishFitLimits{})
	_, err := service.GenerateCall(context.Background(), account, "tok", call)
	if err == nil {
		t.Fatal("expected quota error")
	}
	var insufficient *fish.InsufficientCreditsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want InsufficientCreditsError", err)
	}
	if call.Delivered() != 4 {
		t.Fatalf("delivered = %d, want 4 (progress preserved)", call.Delivered())
	}
	if call.Remaining() != 2 {
		t.Fatalf("remaining = %d, want 2", call.Remaining())
	}
	// 分类器判定可换号 → handler 换号续跑
	if !isFishFailoverClass(err) {
		t.Fatal("quota error must be failover-class")
	}
}

func TestFishGenerateCallCrossAccountContinuation(t *testing.T) {
	var submits int32
	service, _, _ := newFishTestService(t, fishSplitUpstream(t, &submits, 3))
	free := fishAccountWithCap(2)
	pro := fishAccountWithCap(8)
	call := NewFishImageCall(fishImageRequest("gpt-image-2", "", "", 6), "gpt-image-2", domain.FishFitLimits{})
	// 第一个账号（cap=2）：交 2 组（4 张）后第 3 组 402
	if _, err := service.GenerateCall(context.Background(), free, "tok", call); err == nil {
		t.Fatal("expected quota error on free account")
	}
	if call.Delivered() != 4 {
		t.Fatalf("after free: delivered = %d, want 4", call.Delivered())
	}
	// 第二个账号（cap=8）：只生成缺口 2 张（一次提交）
	before := submits
	result, err := service.GenerateCall(context.Background(), pro, "tok", call)
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if submits != before+1 {
		t.Fatalf("continuation submits = %d (delta %d), want exactly 1 (only the gap)", submits, submits-before)
	}
	if result.Forward.ImageCount != 6 {
		t.Fatalf("final ImageCount = %d, want 6 (aggregated across accounts)", result.Forward.ImageCount)
	}
}

func TestFishBuildPartialResult(t *testing.T) {
	service := NewFishImageService(nil)
	call := NewFishImageCall(fishImageRequest("gpt-image-2", "1024x1024", "medium", 8), "gpt-image-2", domain.FishFitLimits{})
	call.delivered = [][]byte{[]byte("a"), []byte("b")}
	result := service.BuildFishPartialResult(context.Background(), call)
	if result == nil {
		t.Fatal("partial result nil")
	}
	if result.Forward.ImageCount != 2 {
		t.Fatalf("ImageCount = %d, want 2", result.Forward.ImageCount)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body["data"].([]any)) != 2 {
		t.Fatalf("data len mismatch")
	}
	// 空进度 → nil
	empty := NewFishImageCall(fishImageRequest("gpt-image-2", "", "", 2), "gpt-image-2", domain.FishFitLimits{})
	if service.BuildFishPartialResult(context.Background(), empty) != nil {
		t.Fatal("empty call must yield nil")
	}
}

func TestAccountFishImageCap(t *testing.T) {
	if got := accountFishImageCap(fishAccountWithCap(2)); got != 2 {
		t.Fatalf("cap = %d, want 2", got)
	}
	if got := accountFishImageCap(fishAccountWithCap(0)); got != fishMaxImagesPerRequest {
		t.Fatalf("missing cap must fall back to model max, got %d", got)
	}
	invalid := fishAccountWithCap(0)
	invalid.Credentials[fishCredentialMaxPerGen] = "99"
	if got := accountFishImageCap(invalid); got != fishMaxImagesPerRequest {
		t.Fatalf("invalid cap must fall back, got %d", got)
	}
	if got := accountFishImageCap(nil); got != fishMaxImagesPerRequest {
		t.Fatalf("nil account cap = %d", got)
	}
}
