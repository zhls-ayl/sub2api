//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestUpdateAccount_BPSOAuthKeepsType(t *testing.T) {
	accountID := int64(310)
	newRepo := func() *updateAccountOveragesRepoStub {
		return &updateAccountOveragesRepoStub{account: &Account{
			ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeBPSOAuth, Status: StatusActive,
			Credentials: map[string]any{"access_token": "at", "client_id": openai.ExcelClientID},
			Extra:       map[string]any{},
		}}
	}

	// Dropping client_id while switching type must not leave the Excel token
	// on a native account.
	repo := newRepo()
	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Type: AccountTypeOAuth, Credentials: map[string]any{"refresh_token": "rt"},
	})
	require.Error(t, err)
	require.Zero(t, repo.updateCalls)

	repo = newRepo()
	_, err = (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{ExcelBPSIgnoreEncryptedContentKey: true},
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
}
