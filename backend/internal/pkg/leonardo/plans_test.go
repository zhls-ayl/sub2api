package leonardo

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestLookupPlanMirrorsLeonardoFrontendConstants(t *testing.T) {
	cases := map[string]PlanQuota{
		"FREE":     {FastTokens: 150, DisplayName: "Free", DailyFast: true},
		"FREEPLUS": {FastTokens: 8000, DisplayName: "Free Plus", CanUseTopUp: true},
		"BASIC":    {FastTokens: 8500, MaxRolloverTokens: 25500, DisplayName: "Essential", CanUseRollover: true, CanUseTopUp: true},
		"STANDARD": {FastTokens: 25000, MaxRolloverTokens: 75000, DisplayName: "Premium", CanUseRollover: true, CanUseTopUp: true},
		"PRO":      {FastTokens: 60000, MaxRolloverTokens: 180000, DisplayName: "Ultimate", CanUseRollover: true, CanUseTopUp: true},
	}
	for plan, want := range cases {
		got, ok := LookupPlan(plan)
		if !ok || got != want {
			t.Fatalf("LookupPlan(%q) = %+v, %v; want %+v", plan, got, ok, want)
		}
	}
	if got, ok := LookupPlan(" standard "); !ok || got.DisplayName != "Premium" {
		t.Fatalf("lookup should be case-insensitive, got %+v %v", got, ok)
	}
	if _, ok := LookupPlan("ENTERPRISE"); ok {
		t.Fatalf("unknown plan must not resolve")
	}
}

func TestQueryCreditsUsableTokensFollowPlanCapabilities(t *testing.T) {
	cases := []struct {
		plan       string
		wantUsable int
	}{
		// 免费套餐：Rollover 与 Top-up 都不可用。
		{plan: "FREE", wantUsable: 100},
		// 付费套餐：三池都可用。
		{plan: "STANDARD", wantUsable: 100 + 20 + 7},
		// 未知套餐不推断：等于三池之和。
		{plan: "ENTERPRISE", wantUsable: 100 + 20 + 7},
	}
	for _, tc := range cases {
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"user_details": []map[string]any{{
					"plan": tc.plan, "subscriptionTokens": 100, "rolloverTokens": 20, "paidTokens": 7,
				}}},
			})
		})
		credits, err := client.QueryCredits(context.Background(), "jwt-1", "sub-1")
		if err != nil {
			t.Fatalf("%s: QueryCredits error: %v", tc.plan, err)
		}
		if credits.TotalTokens != 127 || credits.UsableTokens != tc.wantUsable {
			t.Fatalf("%s: total=%d usable=%d, want total=127 usable=%d", tc.plan, credits.TotalTokens, credits.UsableTokens, tc.wantUsable)
		}
	}
}
