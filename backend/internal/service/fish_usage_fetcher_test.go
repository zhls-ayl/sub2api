package service

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetFishUsageCachesWalletAndDailyWindow(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/self":
			_, _ = w.Write([]byte(`{"_id":"uid-9","email":"a@b.c"}`))
		case "/wallet/uid-9/package":
			_, _ = w.Write([]byte(`{"total":8000,"balance":6500,"type":"free"}`))
		case "/tools/image-video/entitlements":
			_, _ = w.Write([]byte(`{"plan":"free","maxImagesPerGeneration":2,"dailyImage":{"limit":3,"used":1,"remaining":2,"resetAt":"2099-03-14T00:00:00Z"}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	orig := fishUsageAPIBaseForTest
	fishUsageAPIBaseForTest = server.URL
	t.Cleanup(func() { fishUsageAPIBaseForTest = orig })

	svc := &AccountUsageService{cache: NewUsageCache()}
	account := &Account{
		ID:          7,
		Platform:    PlatformFish,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{fishCredentialToken: "tok"},
	}

	info, err := svc.getFishUsage(t.Context(), account, "passive", false)
	if err != nil {
		t.Fatalf("getFishUsage: %v", err)
	}
	if info.FishPlan != "free" {
		t.Fatalf("plan = %q, want free", info.FishPlan)
	}
	if info.FishCredits == nil || info.FishCredits.CurrentUsage != 1500 || info.FishCredits.UsageLimit != 8000 {
		t.Fatalf("credits = %+v, want used 1500 / 8000", info.FishCredits)
	}
	if info.FishDaily == nil || info.FishDaily.CurrentUsage != 1 || info.FishDaily.UsageLimit != 3 {
		t.Fatalf("daily = %+v, want 1 / 3", info.FishDaily)
	}
	if info.FishResetAt == nil || !info.FishResetAt.Equal(time.Date(2099, 3, 14, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset = %v", info.FishResetAt)
	}
	firstHits := hits.Load()

	if _, err := svc.getFishUsage(t.Context(), account, "passive", false); err != nil {
		t.Fatalf("cached getFishUsage: %v", err)
	}
	if hits.Load() != firstHits {
		t.Fatalf("cache miss: hits %d, want %d", hits.Load(), firstHits)
	}

	if _, err := svc.getFishUsage(t.Context(), account, "active", true); err != nil {
		t.Fatalf("force getFishUsage: %v", err)
	}
	if hits.Load() == firstHits {
		t.Fatal("force refresh did not hit upstream")
	}
}

func TestGetFishUsageErrorCacheSkipsForce(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid token"}`))
	}))
	t.Cleanup(server.Close)

	orig := fishUsageAPIBaseForTest
	fishUsageAPIBaseForTest = server.URL
	t.Cleanup(func() { fishUsageAPIBaseForTest = orig })

	svc := &AccountUsageService{cache: NewUsageCache()}
	account := &Account{
		ID:          8,
		Platform:    PlatformFish,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{fishCredentialToken: "tok"},
	}

	info, err := svc.getFishUsage(t.Context(), account, "active", true)
	if err != nil {
		t.Fatalf("getFishUsage: %v", err)
	}
	if !info.NeedsReauth || info.ErrorCode != errorCodeUnauthenticated {
		t.Fatalf("degraded = %+v", info)
	}
	firstHits := hits.Load()

	again, err := svc.getFishUsage(t.Context(), account, "active", true)
	if err != nil {
		t.Fatalf("cached error: %v", err)
	}
	if hits.Load() != firstHits {
		t.Fatalf("error cache bypassed on force: hits %d, want %d", hits.Load(), firstHits)
	}
	if again.Error == "" {
		t.Fatal("expected cached error")
	}
}
