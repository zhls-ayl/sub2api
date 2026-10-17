//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateLeonardoCreditUnitPriceFromExtra(t *testing.T) {
	require.NoError(t, ValidateLeonardoCreditUnitPriceFromExtra(nil))
	require.NoError(t, ValidateLeonardoCreditUnitPriceFromExtra(map[string]any{}))

	extra := map[string]any{"leonardo_credit_unit_price_usd": "0.0012"}
	require.NoError(t, ValidateLeonardoCreditUnitPriceFromExtra(extra))
	require.Equal(t, 0.0012, extra["leonardo_credit_unit_price_usd"])

	err := ValidateLeonardoCreditUnitPriceFromExtra(map[string]any{"leonardo_credit_unit_price_usd": -1})
	require.ErrorContains(t, err, "leonardo_credit_unit_price_usd must be a finite number >= 0")
	require.Error(t, ValidateLeonardoCreditUnitPriceFromExtra(map[string]any{"leonardo_credit_unit_price_usd": "nope"}))

	// Kiro 的 key 不受 Leonardo 校验影响，反之亦然。
	require.NoError(t, ValidateLeonardoCreditUnitPriceFromExtra(map[string]any{"kiro_credit_unit_price_usd": -1}))
	require.NoError(t, ValidateKiroCreditUnitPriceFromExtra(map[string]any{"leonardo_credit_unit_price_usd": -1}))
}

func TestUpdateAccount_ValidatesLeonardoCreditUnitPriceExtra(t *testing.T) {
	accountID := int64(301)
	repo := &updateAccountOveragesRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformLeonardo,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra:    map[string]any{},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"leonardo_credit_unit_price_usd": -0.1},
	})
	require.Error(t, err)
	require.Zero(t, repo.updateCalls)

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"leonardo_credit_unit_price_usd": "0.0012"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.Equal(t, 0.0012, updated.Extra["leonardo_credit_unit_price_usd"])
}

func TestCreateAccount_ValidatesLeonardoCreditUnitPriceExtra(t *testing.T) {
	repo := &createAccountKiroCreditRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "leonardo-cookie",
		Platform:             PlatformLeonardo,
		Type:                 AccountTypeOAuth,
		SkipDefaultGroupBind: true,
		Extra:                map[string]any{"leonardo_credit_unit_price_usd": -0.1},
	})
	require.Error(t, err)
	require.Zero(t, repo.createCalls)

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "leonardo-cookie",
		Platform:             PlatformLeonardo,
		Type:                 AccountTypeOAuth,
		SkipDefaultGroupBind: true,
		Extra:                map[string]any{"leonardo_credit_unit_price_usd": 0.001},
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.createCalls)
	require.Equal(t, 0.001, created.Extra["leonardo_credit_unit_price_usd"])
}

func TestUpdateAccount_ValidatesLeonardoImageLimitsExtra(t *testing.T) {
	accountID := int64(302)
	repo := &updateAccountOveragesRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformLeonardo,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra:    map[string]any{},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"leonardo_max_short_edge": 100},
	})
	require.Error(t, err)
	require.Zero(t, repo.updateCalls)

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			"leonardo_max_quality":        "MEDIUM",
			"leonardo_max_short_edge":     "1024",
			"leonardo_max_upscale_factor": 2,
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.Equal(t, "medium", updated.Extra["leonardo_max_quality"])
	require.Equal(t, 1024, updated.Extra["leonardo_max_short_edge"])
}

func TestCreateAccount_ValidatesLeonardoImageLimitsExtra(t *testing.T) {
	repo := &createAccountKiroCreditRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "leonardo-cookie",
		Platform:             PlatformLeonardo,
		Type:                 AccountTypeOAuth,
		SkipDefaultGroupBind: true,
		Extra:                map[string]any{"leonardo_max_quality": "ultra"},
	})
	require.Error(t, err)
	require.Zero(t, repo.createCalls)
}
