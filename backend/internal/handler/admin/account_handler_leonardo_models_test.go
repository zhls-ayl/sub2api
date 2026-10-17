package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func leonardoAvailableModelIDs(t *testing.T, account service.Account) []string {
	t.Helper()
	router := setupAvailableModelsRouter(&availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account:          account,
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/61/models", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Data))
	for _, model := range resp.Data {
		ids = append(ids, model.ID)
	}
	return ids
}

// Leonardo Cookie 号是 oauth 类型，不能落到 Claude 默认模型。
func TestAccountHandlerGetAvailableModels_LeonardoCookieUsesLeonardoModels(t *testing.T) {
	ids := leonardoAvailableModelIDs(t, service.Account{
		ID:          61,
		Platform:    service.PlatformLeonardo,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Credentials: map[string]any{"cookie": "c"},
	})
	require.Equal(t, []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}, ids)
}

func TestAccountHandlerGetAvailableModels_LeonardoMappingKeepsOnlyServableModels(t *testing.T) {
	ids := leonardoAvailableModelIDs(t, service.Account{
		ID:       61,
		Platform: service.PlatformLeonardo,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"base_url": "https://relay.example.com",
			"api_key":  "sk-x",
			"model_mapping": map[string]any{
				"my-image":            "gpt-image-2",
				"gpt-image-2.5-flare": "gpt-image-2.5-flare",
				"slug-image":          "openai/gpt-image-2.5-flare",
				"unknown-image":       "gpt-image-9",
				"gpt-image-*":         "gpt-image-2",
			},
		},
	})
	// 中转号：通配符、映射到上游 slug 或未知模型的条目都不列出。
	require.Equal(t, []string{"gpt-image-2.5-flare", "my-image"}, ids)
}
