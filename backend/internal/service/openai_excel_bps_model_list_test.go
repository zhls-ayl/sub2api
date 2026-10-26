package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

func excelBPSListModelIDs(models []openai.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// A BPS-only account lists its mapping keys (sorted, wildcards skipped), or
// the default BPS models while its model restriction is empty. A legacy
// openai_excel_bps_models whitelist has no effect.
func TestExcelBPSConfiguredModels(t *testing.T) {
	empty := excelAccount()
	empty.Extra["openai_excel_bps_models"] = []any{"gpt-5.6-sol"}
	require.Equal(t, ExcelBPSDefaultModels, excelBPSListModelIDs(ExcelBPSConfiguredModels(empty)))

	mapped := excelAccount()
	mapped.Credentials["model_mapping"] = map[string]any{"z-sol": "gpt-5.6-sol", "a-terra": "gpt-5.6-terra", "x-*": "gpt-5.6-sol"}
	for i := 0; i < 5; i++ {
		models := ExcelBPSConfiguredModels(mapped)
		require.Equal(t, []string{"a-terra", "z-sol"}, excelBPSListModelIDs(models))
		for _, model := range models {
			require.NotEmpty(t, model.DisplayName)
		}
	}
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-5.6-terra"}, excelBPSUpstreamModelIDs(mapped))

	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		account := &Account{ID: 9, Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5"}}}
		require.Empty(t, ExcelBPSConfiguredModels(account))
	}
}

func excelBPSListingService(accounts ...Account) (*GatewayService, *int64) {
	groupID := int64(77)
	svc := &GatewayService{
		accountRepo:        &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: accounts}},
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}
	return svc, &groupID
}

// BPS-only accounts contribute their configured models to /v1/models: the
// mapping keys, or the default BPS models, never the native defaults.
func TestGetAvailableModelsListsExcelBPSModels(t *testing.T) {
	bps := *excelAccount()
	bps.Credentials["model_mapping"] = map[string]any{"my-sol": "gpt-5.6-sol"}
	svc, groupID := excelBPSListingService(bps)
	require.Equal(t, []string{"my-sol"}, svc.GetAvailableModels(context.Background(), groupID, PlatformOpenAI))

	unmapped := *excelAccount()
	svc, groupID = excelBPSListingService(unmapped)
	got := svc.GetAvailableModels(context.Background(), groupID, PlatformOpenAI)
	require.Equal(t, dedupeAndSortModelIDs(append([]string{}, ExcelBPSDefaultModels...)), got)
	for _, model := range got {
		require.NotContains(t, model, "gpt-image")
		require.NotEqual(t, "codex-auto-review", model)
	}
}

// In a mixed pool the native accounts contribute exactly what they would as a
// pool of their own.
func TestGetAvailableModelsKeepsNativeContribution(t *testing.T) {
	nativeMapped := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"configured-model": "gpt-5"}}}
	nativeUnmapped := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	bps := *excelAccount()
	bps.Credentials["model_mapping"] = map[string]any{"gpt-bps-only": "gpt-bps-only"}
	for name, natives := range map[string][]Account{
		"mapped":   {nativeMapped},
		"unmapped": {nativeUnmapped},
		"both":     {nativeMapped, nativeUnmapped},
	} {
		t.Run(name, func(t *testing.T) {
			pureSvc, pureGroup := excelBPSListingService(natives...)
			pure := pureSvc.GetAvailableModels(context.Background(), pureGroup, PlatformOpenAI)
			if pure == nil {
				pure = filterRetiredOpenAIModelIDs(natives, openai.DefaultModelIDs())
			}

			mixedSvc, mixedGroup := excelBPSListingService(append(append([]Account{}, natives...), bps)...)
			mixed := mixedSvc.GetAvailableModels(context.Background(), mixedGroup, PlatformOpenAI)
			require.Equal(t, dedupeAndSortModelIDs(append(append([]string{}, pure...), "gpt-bps-only")), mixed)
		})
	}
}
