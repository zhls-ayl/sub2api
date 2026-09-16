package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Codex manifest 标准化后只保留 slug，测试弹窗用 display_name 当选项标签，
// 留空会让模型选择器渲染成一排空白项。
func TestFetchOpenAIAccountModelsFillsPickerLabels(t *testing.T) {
	newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"gpt-5.6-terra"},{"slug":"codex-auto-review"}]}`)
	svc := &AccountTestService{}
	svc.SetOpenAIGatewayService(&OpenAIGatewayService{})

	models, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsTestAccount())
	require.NoError(t, err)
	require.Len(t, models, 2)
	for _, model := range models {
		require.NotEmpty(t, model.DisplayName, "picker label must not be empty for %q", model.ID)
		require.Equal(t, model.ID, model.DisplayName)
		require.Equal(t, "model", model.Type)
	}
	require.Equal(t, "gpt-5.6-terra", models[0].ID)
	require.Equal(t, "codex-auto-review", models[1].ID)
}
