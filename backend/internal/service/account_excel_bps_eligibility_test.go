package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

// BPS is the only channel of a BPS-only account, so it is on by type: there is
// no per-account switch, and no other account type ever routes through BPS.
func TestIsExcelBPSEnabledFollowsBPSOAuthType(t *testing.T) {
	require.True(t, excelAccount().IsExcelBPSEnabled())
	require.True(t, excelAccount().IsExcelBPSEnabledForModel("gpt-5.6-sol"))

	codex := excelAccount()
	codex.Type = AccountTypeOAuth
	codex.Extra["openai_excel_bps"] = true
	require.False(t, codex.IsExcelBPSEnabled(), "a Codex OAuth account never routes through BPS")
	require.False(t, codex.IsBPSOAuth())

	free := excelAccount()
	free.Credentials["plan_type"] = "free"
	require.False(t, free.IsExcelBPSEnabled(), "free plans stay ineligible")

	parentID := int64(300)
	shadow := excelAccount()
	shadow.ParentAccountID = &parentID
	require.False(t, shadow.IsExcelBPSEnabled())
}

// A BPS-only account serves what its model restriction names, or the three
// default BPS models while it is empty. Passthrough never widens it, and a
// legacy openai_excel_bps_models whitelist no longer has any effect.
func TestBPSOAuthModelSupportFollowsModelRestriction(t *testing.T) {
	empty := excelAccount()
	for _, model := range ExcelBPSDefaultModels {
		require.True(t, empty.IsModelSupported(model), model)
	}
	for _, model := range []string{"gpt-5.5", "gpt-6.1-sol", "gpt-image-2", "codex-auto-review", "my-alias"} {
		require.False(t, empty.IsModelSupported(model), model)
	}
	require.True(t, empty.IsExcelBPSEnabledForModel("gpt-5.5"), "every model of an eligible account routes through BPS")

	whitelist := excelAccount()
	whitelist.Credentials["model_mapping"] = map[string]any{"gpt-7-new": "gpt-7-new"}
	whitelist.Extra["openai_excel_bps_models"] = []any{"gpt-5.6-sol"}
	require.True(t, whitelist.IsModelSupported("gpt-7-new"))
	require.False(t, whitelist.IsModelSupported("gpt-5.6-sol"), "a configured restriction replaces the defaults")

	alias := excelAccount()
	alias.Credentials["model_mapping"] = map[string]any{"my-sol": "gpt-5.6-sol"}
	require.True(t, alias.IsModelSupported("my-sol"))
	require.False(t, alias.IsModelSupported("gpt-6-astra"))

	free := excelAccount()
	free.Credentials["plan_type"] = "free"
	require.False(t, free.IsModelSupported("gpt-5.6-sol"))
	require.False(t, free.IsExcelBPSEnabledForModel("gpt-5.6-sol"))

	native := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk"}}
	require.True(t, native.IsModelSupported("gpt-5.5"), "other account types keep their own rules")
}

func TestIsOpenAIOAuthCredentialAccountCoversBPSOAuth(t *testing.T) {
	require.True(t, excelAccount().IsOpenAIOAuthCredentialAccount())
	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).IsOpenAIOAuthCredentialAccount())
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).IsOpenAIOAuthCredentialAccount())
	require.False(t, (&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}).IsOpenAIOAuthCredentialAccount())
	require.Equal(t, "test-token", excelAccount().GetOpenAIAccessToken())
}

// The account type and the OAuth client must agree: BPS accepts Excel-client
// tokens only, and an Excel token can never serve native Codex.
func TestValidateBPSOAuthCredentials(t *testing.T) {
	excel := map[string]any{"client_id": openai.ExcelClientID}
	codex := map[string]any{"client_id": openai.ClientID}
	for _, tc := range []struct {
		name        string
		platform    string
		accountType string
		credentials map[string]any
		wantErr     bool
	}{
		{"bps-oauth with Excel client", PlatformOpenAI, AccountTypeBPSOAuth, excel, false},
		{"bps-oauth without Excel client", PlatformOpenAI, AccountTypeBPSOAuth, codex, true},
		{"bps-oauth without client_id", PlatformOpenAI, AccountTypeBPSOAuth, map[string]any{}, true},
		{"bps-oauth on another platform", PlatformAnthropic, AccountTypeBPSOAuth, excel, true},
		{"oauth with Excel client", PlatformOpenAI, AccountTypeOAuth, excel, true},
		{"oauth with Codex client", PlatformOpenAI, AccountTypeOAuth, codex, false},
		{"setup-token with Excel client", PlatformOpenAI, AccountTypeSetupToken, excel, true},
		{"setup-token without client_id", PlatformOpenAI, AccountTypeSetupToken, map[string]any{}, false},
		{"apikey", PlatformOpenAI, AccountTypeAPIKey, map[string]any{"api_key": "sk"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBPSOAuthCredentials(tc.platform, tc.accountType, tc.credentials)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOpenAITokenRefresherCanRefreshBPSOAuth(t *testing.T) {
	refresher := &OpenAITokenRefresher{}
	require.True(t, refresher.CanRefresh(excelAccount()))
	require.True(t, refresher.CanRefresh(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}))
	require.False(t, refresher.CanRefresh(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}))
}

func TestGetAccessTokenReadsBPSOAuthCredentials(t *testing.T) {
	svc := &OpenAIGatewayService{}
	token, mode, err := svc.GetAccessToken(t.Context(), excelAccount())
	require.NoError(t, err)
	require.Equal(t, "test-token", token)
	require.Equal(t, "oauth", mode)
}

// A BPS-only account keeps its type: an update may drop client_id while the
// stored Excel access token stays.
func TestValidateBPSOAuthTypeChange(t *testing.T) {
	require.NoError(t, validateBPSOAuthTypeChange(AccountTypeBPSOAuth, ""))
	require.NoError(t, validateBPSOAuthTypeChange(AccountTypeBPSOAuth, AccountTypeBPSOAuth))
	require.NoError(t, validateBPSOAuthTypeChange(AccountTypeOAuth, AccountTypeSetupToken))
	require.Error(t, validateBPSOAuthTypeChange(AccountTypeBPSOAuth, AccountTypeOAuth))
	require.Error(t, validateBPSOAuthTypeChange(AccountTypeBPSOAuth, AccountTypeSetupToken))
	require.Error(t, validateBPSOAuthTypeChange(AccountTypeOAuth, AccountTypeBPSOAuth))
}
