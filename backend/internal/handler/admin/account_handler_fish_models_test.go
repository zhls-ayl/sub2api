package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func fishAvailableModels(t *testing.T, account service.Account) []struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
} {
	t.Helper()
	router := setupAvailableModelsRouter(&availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account:          account,
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/71/models", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Data
}

func TestAccountHandlerGetAvailableModels_FishUsesFishModels(t *testing.T) {
	models := fishAvailableModels(t, service.Account{
		ID:          71,
		Platform:    service.PlatformFish,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"api_key": "tok"},
	})
	require.Equal(t, []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}{
		{ID: "gpt-image-2", DisplayName: "GPT Image 2"},
		{ID: "gpt-image-2.5-flare", DisplayName: "GPT Image 2.5 Flare"},
		{ID: "gpt-image-2.5-sunburst", DisplayName: "GPT Image 2.5 Sunburst"},
	}, models)
}

func TestAccountHandlerGetAvailableModels_FishMappingKeepsOnlyServableModels(t *testing.T) {
	models := fishAvailableModels(t, service.Account{
		ID:       71,
		Platform: service.PlatformFish,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"base_url": "https://relay.example.com",
			"api_key":  "sk-x",
			"model_mapping": map[string]any{
				"my-image":            "gpt-image-2",
				"gpt-image-2.5-flare": "gpt-image-2.5-flare",
				"not-fish":            "gpt-image-9",
				"gpt-image-*":         "gpt-image-2",
			},
		},
	})
	require.Equal(t, []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}{
		{ID: "gpt-image-2.5-flare", DisplayName: "GPT Image 2.5 Flare"},
		{ID: "my-image", DisplayName: "my-image"},
	}, models)
}
