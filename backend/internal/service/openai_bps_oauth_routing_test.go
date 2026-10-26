package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsRoutingNativeAccount(id int64, priority int) Account {
	return Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: priority,
		Credentials: map[string]any{"access_token": "native-token"}}
}

func bpsRoutingBPSAccount(id int64, models ...string) Account {
	account := *excelAccount()
	account.ID = id
	account.Priority = 0
	if len(models) > 0 {
		mapping := make(map[string]any, len(models))
		for _, model := range models {
			mapping[model] = model
		}
		account.Credentials["model_mapping"] = mapping
	}
	return account
}

// BPS has no token-counting endpoint: an Excel-client token must never reach
// the native api.openai.com/v1/responses/input_tokens preflight.
func TestForwardResponsesInputTokensBPSOAuthUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil)
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}

	err := svc.ForwardResponsesInputTokens(context.Background(), c, excelAccount(), []byte(`{"model":"gpt-5.6-sol","input":"hello world"}`))

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "response.input_tokens", gjson.Get(recorder.Body.String(), "object").String())
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.Nil(t, upstream.lastReq)
	require.True(t, shouldEstimateOpenAIInputTokensLocally(excelAccount()))
}

func TestForwardCountTokensAsAnthropicBPSOAuthUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello world"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}

	err := svc.ForwardCountTokensAsAnthropic(context.Background(), c, excelAccount(), body, "gpt-5.6-sol")

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Positive(t, gjson.Get(recorder.Body.String(), "input_tokens").Int())
	require.Nil(t, upstream.lastReq)
}

// The legacy scheduler applies the same BPS-only rules as the advanced one:
// entry points without a BPS route, the BPS model whitelist and the global
// switch all keep a BPS-only account out, while model-less listing still
// reaches it.
func TestLegacySelectionAppliesBPSOAuthRules(t *testing.T) {
	newSvc := func(accounts ...Account) *OpenAIGatewayService {
		return &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: accounts}, cache: &stubGatewayCache{}, cfg: &config.Config{}}
	}
	ctx := context.Background()

	t.Run("BPS account eligible on the Responses entry point", func(t *testing.T) {
		svc := newSvc(bpsRoutingNativeAccount(2, 5), bpsRoutingBPSAccount(1))
		account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.6-sol", nil)
		require.NoError(t, err)
		require.Equal(t, int64(1), account.ID)
	})

	t.Run("entry point without a BPS route", func(t *testing.T) {
		svc := newSvc(bpsRoutingNativeAccount(2, 5), bpsRoutingBPSAccount(1))
		account, err := svc.SelectAccountForModelWithExclusions(WithBPSUnsupportedInbound(ctx), nil, "", "gpt-5.6-sol", nil)
		require.NoError(t, err)
		require.Equal(t, int64(2), account.ID)
	})

	t.Run("token count on a marked entry point", func(t *testing.T) {
		svc := newSvc(bpsRoutingNativeAccount(2, 5), bpsRoutingBPSAccount(1))
		account, err := svc.SelectAccountForTokenCount(WithBPSUnsupportedInbound(ctx), nil, "", "gpt-5.6-sol", OpenAIEndpointCapabilityChatCompletions, PlatformOpenAI)
		require.NoError(t, err)
		require.Equal(t, int64(2), account.ID)
	})

	t.Run("model outside the BPS whitelist", func(t *testing.T) {
		svc := newSvc(bpsRoutingNativeAccount(2, 5), bpsRoutingBPSAccount(1, "gpt-7-new"))
		account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.6-sol", nil)
		require.NoError(t, err)
		require.Equal(t, int64(2), account.ID)
	})

	t.Run("model-less listing keeps a whitelisted BPS account", func(t *testing.T) {
		svc := newSvc(bpsRoutingNativeAccount(2, 5), bpsRoutingBPSAccount(1, "gpt-7-new"))
		account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "", nil)
		require.NoError(t, err)
		require.Equal(t, int64(1), account.ID)
	})
}

// bpsCooldownCases cool the BPS candidate down in the two ways the candidate
// pool does not filter: the account's BPS 429 cooldown and the model
// permission cooldown.
var bpsCooldownCases = []struct {
	name string
	cool func(svc *OpenAIGatewayService, bps *Account)
}{
	{name: "BPS 429 cooldown", cool: func(svc *OpenAIGatewayService, bps *Account) {
		svc.coolDownExcelBPS(context.Background(), bps, "60")
	}},
	{name: "model permission cooldown", cool: func(svc *OpenAIGatewayService, bps *Account) {
		svc.coolDownExcelBPSModel(bps, "gpt-5.6-sol")
	}},
}

// A sticky session stays on its native account even when a BPS account could
// serve the model: BPS and native accounts compete on equal terms, and only the
// sticky account's own eligibility can move the session.
func TestLegacyStickyBindingStaysOnNativeAccount(t *testing.T) {
	const sessionHash = "bps-sticky"
	for name, ctx := range map[string]context.Context{
		"Responses":               context.Background(),
		"unsupported entry point": WithBPSUnsupportedInbound(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			bps := bpsRoutingBPSAccount(1)
			cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:" + sessionHash: 2}}
			svc := &OpenAIGatewayService{
				accountRepo: stubOpenAIAccountRepo{accounts: []Account{bpsRoutingNativeAccount(2, 5), bpsRoutingNativeAccount(3, 5), bps}},
				cache:       cache,
				cfg:         &config.Config{},
			}

			account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, sessionHash, "gpt-5.6-sol", nil)

			require.NoError(t, err)
			require.Equal(t, int64(2), account.ID)
			require.Zero(t, cache.deletedSessions["openai:"+sessionHash])
		})
	}
}

// Forward-time backstop: a BPS-only account that cannot serve the request
// hands it to another account instead of answering 400.
func TestForwardBPSOAuthRouteUnavailableFailsOver(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account *Account
		svc     func(*httpUpstreamRecorder) *OpenAIGatewayService
	}{
		{name: "free plan", account: func() *Account {
			account := excelAccount()
			account.Credentials["plan_type"] = "free"
			return account
		}(), svc: openAIClientToolsTestService},
		{name: "model outside the whitelist", account: func() *Account {
			account := bpsRoutingBPSAccount(300, "gpt-7-new")
			return &account
		}(), svc: openAIClientToolsTestService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upstream := &httpUpstreamRecorder{}
			request, err := buildExcelBPSAccountTestBody("gpt-5.6-sol", "hi")
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(request))

			_, err = tc.svc(upstream).Forward(context.Background(), c, tc.account, request)

			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover))
			require.Equal(t, ExcelOAuthRouteUnavailableReason, failover.Reason)
			require.True(t, failover.ShouldRetryNextAccount())
			require.Empty(t, upstream.requests)
			require.Zero(t, rec.Body.Len(), "leave the response to the handler")
		})
	}
}

// The advanced scheduler's sticky layer keeps native sessions too.
func TestAdvancedStickyBindingStaysOnNativeAccount(t *testing.T) {
	groupID := int64(10)
	native := bpsRoutingNativeAccount(2001, 5)
	native.GroupIDs = []int64{groupID}
	bps := bpsRoutingBPSAccount(2002)
	bps.GroupIDs = []int64{groupID}
	for name, ctx := range map[string]context.Context{
		"Responses":               context.Background(),
		"unsupported entry point": WithBPSUnsupportedInbound(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:bps_sticky": native.ID}}
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{native, bps}},
				cache:              cache,
				cfg:                &config.Config{},
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}

			selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "bps_sticky", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)

			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, native.ID, selection.Account.ID)
			require.True(t, decision.StickySessionHit)
			require.Zero(t, cache.deletedSessions["openai:bps_sticky"])
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

// A sticky binding to a BPS account must not bypass that account's in-memory
// BPS cooldowns: the session moves to another account while they last.
func TestLegacyStickyBindingToCoolingBPSAccountMoves(t *testing.T) {
	const sessionHash = "bps-cooling-sticky"
	for _, cooldown := range bpsCooldownCases {
		t.Run(cooldown.name, func(t *testing.T) {
			bps := bpsRoutingBPSAccount(1)
			cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:" + sessionHash: bps.ID}}
			svc := &OpenAIGatewayService{
				accountRepo: stubOpenAIAccountRepo{accounts: []Account{bps, bpsRoutingNativeAccount(2, 5)}},
				cache:       cache,
				cfg:         &config.Config{},
			}
			cooldown.cool(svc, &bps)

			account, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-5.6-sol", nil)

			require.NoError(t, err)
			require.Equal(t, int64(2), account.ID)
		})
	}
}

func TestAdvancedStickyBindingToCoolingBPSAccountMoves(t *testing.T) {
	groupID := int64(11)
	native := bpsRoutingNativeAccount(2101, 5)
	native.GroupIDs = []int64{groupID}
	bps := bpsRoutingBPSAccount(2102)
	bps.GroupIDs = []int64{groupID}
	for _, cooldown := range bpsCooldownCases {
		t.Run(cooldown.name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:bps_cooling": bps.ID}}
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{native, bps}},
				cache:              cache,
				cfg:                &config.Config{},
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			cooldown.cool(svc, &bps)

			selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "bps_cooling", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)

			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, native.ID, selection.Account.ID)
			require.False(t, decision.StickySessionHit)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

// WebSocket ingress has no BPS route: a BPS-only account that reaches it is
// handed back to the handler for another account, whatever the model or plan,
// and never continues to native Codex.
func TestWSIngressRejectsBPSOAuthAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, tc := range map[string]struct {
		account func() *Account
	}{
		"whitelisted model": {account: excelAccount},
		"model outside the whitelist": {account: func() *Account {
			account := bpsRoutingBPSAccount(300, "gpt-7-new")
			return &account
		}},
		"free plan": {account: func() *Account {
			account := excelAccount()
			account.Credentials["plan_type"] = "free"
			return account
		}},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := openAIClientToolsTestService(upstream)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

			err := svc.ProxyResponsesWebSocketFromClient(context.Background(), c, new(coderws.Conn), tc.account(), "test-token", []byte(`{"type":"response.create","model":"gpt-5.6-sol"}`), nil)

			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, BPSUnsupportedInboundReason, failover.Reason)
			require.True(t, failover.ShouldRetryNextAccount())
			require.Empty(t, upstream.requests)
		})
	}
}

// With every BPS account cooling down, the legacy schedulers name the BPS
// cooldown in "no available accounts" diagnostics, so the handler answers 429
// (rate_limited=N) instead of a generic 503.
func TestLegacySelectionReportsBPSCooldowns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cool  func(svc *OpenAIGatewayService, bps *Account)
		label string
	}{
		{name: "BPS 429 cooldown", cool: bpsCooldownCases[0].cool, label: excelBPSRateLimitedFilterReason + "=2"},
		{name: "model permission cooldown", cool: bpsCooldownCases[1].cool, label: excelBPSModelPermissionFilterReason + "=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newSvc := func() *OpenAIGatewayService {
				first, second := bpsRoutingBPSAccount(1), bpsRoutingBPSAccount(2)
				cfg := &config.Config{RunMode: config.RunModeStandard}
				cfg.Gateway.Scheduling.LoadBatchEnabled = true
				svc := &OpenAIGatewayService{
					accountRepo:        stubOpenAIAccountRepo{accounts: []Account{first, second}},
					cache:              &stubGatewayCache{},
					cfg:                cfg,
					concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
				}
				tc.cool(svc, &first)
				tc.cool(svc, &second)
				return svc
			}

			_, err := newSvc().SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-5.6-sol", nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.label, "selectBestAccount")

			_, err = newSvc().SelectAccountWithLoadAwareness(context.Background(), nil, "", "gpt-5.6-sol", nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.label, "load-aware selection")
		})
	}
}

// listCountingOpenAIAccountRepo counts how often the scheduler lists the pool.
type listCountingOpenAIAccountRepo struct {
	stubOpenAIAccountRepo
	lists *int
}

func (r listCountingOpenAIAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	*r.lists++
	return r.stubOpenAIAccountRepo.ListSchedulableUngroupedByPlatform(ctx, platform)
}

type listCountingSchedulerRepo struct {
	schedulerTestOpenAIAccountRepo
	lists *int
}

func (r listCountingSchedulerRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	*r.lists++
	return r.schedulerTestOpenAIAccountRepo.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
}

// A sticky hit never lists the pool, with or without BPS-only accounts in it:
// there is no BPS preference to check.
func TestLegacyStickyHitNeverListsPool(t *testing.T) {
	const sessionHash = "native-sticky"
	lists := 0
	svc := &OpenAIGatewayService{
		accountRepo: listCountingOpenAIAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{bpsRoutingNativeAccount(2, 5), bpsRoutingNativeAccount(3, 5), bpsRoutingBPSAccount(1)}}, lists: &lists},
		cache:       &stubGatewayCache{sessionBindings: map[string]int64{"openai:" + sessionHash: 2}},
		cfg:         &config.Config{},
	}
	for i := 0; i < 5; i++ {
		account, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-5.6-sol", nil)
		require.NoError(t, err)
		require.Equal(t, int64(2), account.ID)
	}
	require.Zero(t, lists)
}

func TestAdvancedStickyHitNeverListsPool(t *testing.T) {
	groupID := int64(12)
	native := bpsRoutingNativeAccount(2201, 5)
	native.GroupIDs = []int64{groupID}
	other := bpsRoutingNativeAccount(2202, 5)
	other.GroupIDs = []int64{groupID}
	bps := bpsRoutingBPSAccount(2203)
	bps.GroupIDs = []int64{groupID}
	lists := 0
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc := &OpenAIGatewayService{
		accountRepo:        listCountingSchedulerRepo{schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{native, other, bps}}, lists: &lists},
		cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:native_sticky": native.ID}},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	for i := 0; i < 5; i++ {
		selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "native_sticky", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.Equal(t, native.ID, selection.Account.ID)
		require.True(t, decision.StickySessionHit)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
	require.Zero(t, lists)
}

// BPS-only and native accounts compete on the same terms in the legacy
// scheduler: priority decides across types and nothing favors BPS at equal
// priority (the least recently used account wins, as between native accounts).
func TestLegacySelectionTreatsBPSAndNativeEqually(t *testing.T) {
	recent := time.Now()
	for name, tc := range map[string]struct {
		nativePriority, bpsPriority int
		nativeUsed, bpsUsed         *time.Time
		want                        int64
	}{
		"native wins on priority":              {nativePriority: 1, bpsPriority: 5, want: 2},
		"BPS wins on priority":                 {nativePriority: 5, bpsPriority: 1, want: 1},
		"equal priority, BPS used more recent": {nativePriority: 3, bpsPriority: 3, bpsUsed: &recent, want: 2},
		"equal priority, native used recently": {nativePriority: 3, bpsPriority: 3, nativeUsed: &recent, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			native := bpsRoutingNativeAccount(2, tc.nativePriority)
			native.LastUsedAt = tc.nativeUsed
			bps := bpsRoutingBPSAccount(1)
			bps.Priority = tc.bpsPriority
			bps.LastUsedAt = tc.bpsUsed
			svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{native, bps}}, cache: &stubGatewayCache{}, cfg: &config.Config{}}

			account, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-5.6-sol", nil)

			require.NoError(t, err)
			require.Equal(t, tc.want, account.ID)
		})
	}
}

// The advanced scheduler likewise ranks BPS-only and native accounts together
// by priority, score and load.
func TestAdvancedSelectionTreatsBPSAndNativeEqually(t *testing.T) {
	groupID := int64(13)
	for name, tc := range map[string]struct {
		nativePriority, bpsPriority int
		nativeLoad, bpsLoad         int
		want                        int64
	}{
		"native wins on priority":        {nativePriority: 1, bpsPriority: 5, want: 2301},
		"BPS wins on priority":           {nativePriority: 5, bpsPriority: 1, want: 2302},
		"equal priority, lighter load":   {nativePriority: 3, bpsPriority: 3, bpsLoad: 90, want: 2301},
		"equal priority, BPS is lighter": {nativePriority: 3, bpsPriority: 3, nativeLoad: 90, want: 2302},
	} {
		t.Run(name, func(t *testing.T) {
			native := bpsRoutingNativeAccount(2301, tc.nativePriority)
			native.GroupIDs = []int64{groupID}
			bps := bpsRoutingBPSAccount(2302)
			bps.Priority = tc.bpsPriority
			bps.GroupIDs = []int64{groupID}
			svc := &OpenAIGatewayService{
				accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{native, bps}},
				cache:            &schedulerTestGatewayCache{},
				cfg:              newSchedulerTestSubscriptionPriorityConfig(),
				rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
					acquireResults: map[int64]bool{native.ID: true, bps.ID: true},
					loadMap: map[int64]*AccountLoadInfo{
						native.ID: {AccountID: native.ID, LoadRate: tc.nativeLoad},
						bps.ID:    {AccountID: bps.ID, LoadRate: tc.bpsLoad},
					},
				}),
			}

			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)

			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, tc.want, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

// Subscription priority applies to BPS-whitelisted models as well: the
// subscription pool is tried before regular accounts, BPS-only ones included.
func TestAdvancedSubscriptionPriorityAppliesToBPSModels(t *testing.T) {
	groupID := int64(14)
	subscription := bpsRoutingNativeAccount(2401, 10)
	subscription.GroupIDs = []int64{groupID}
	subscription.Credentials["plan_type"] = "plus"
	bps := bpsRoutingBPSAccount(2402)
	bps.Priority = 0
	bps.GroupIDs = []int64{groupID}
	svc := &OpenAIGatewayService{
		accountRepo:      schedulerTestOpenAIAccountRepo{accounts: []Account{subscription, bps}},
		cache:            &schedulerTestGatewayCache{},
		cfg:              newSchedulerTestSubscriptionPriorityConfig(),
		rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "", "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{subscription.ID: true, bps.ID: true},
		}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, subscription.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// Pools without BPS-only accounts are untouched: priority decides in both
// schedulers and sticky sessions stay where they are.
func TestNativePoolSelectionUnchanged(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		svc := &OpenAIGatewayService{
			accountRepo: stubOpenAIAccountRepo{accounts: []Account{bpsRoutingNativeAccount(2, 5), bpsRoutingNativeAccount(3, 1)}},
			cache:       &stubGatewayCache{sessionBindings: map[string]int64{"openai:native": 2}},
			cfg:         &config.Config{},
		}
		account, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-5.6-sol", nil)
		require.NoError(t, err)
		require.Equal(t, int64(3), account.ID)
		account, err = svc.SelectAccountForModelWithExclusions(context.Background(), nil, "native", "gpt-5.6-sol", nil)
		require.NoError(t, err)
		require.Equal(t, int64(2), account.ID, "the sticky session stays")
	})
	t.Run("advanced", func(t *testing.T) {
		groupID := int64(15)
		low := bpsRoutingNativeAccount(2501, 5)
		low.GroupIDs = []int64{groupID}
		high := bpsRoutingNativeAccount(2502, 1)
		high.GroupIDs = []int64{groupID}
		svc := &OpenAIGatewayService{
			accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{low, high}},
			cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:native": low.ID}},
			cfg:                newSchedulerTestSubscriptionPriorityConfig(),
			rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{low.ID: true, high.ID: true}}),
		}
		selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.Equal(t, high.ID, selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "native", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.Equal(t, low.ID, selection.Account.ID)
		require.True(t, decision.StickySessionHit)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})
}
