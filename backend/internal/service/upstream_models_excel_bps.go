package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// fetchExcelBPSUpstreamModels syncs a BPS-only account from the same BPS
// access catalog the gateway serves for /v1/models and Codex models; native
// discovery would send the Excel-client token to a route it cannot use. The
// catalog is not narrowed by the account's BPS model whitelist: sync exists to
// show what the upstream offers.
func (s *AccountTestService) fetchExcelBPSUpstreamModels(ctx context.Context, account *Account) ([]string, []byte, error) {
	if s.openaiGatewayService == nil {
		return nil, nil, newUpstreamModelSyncConfigError("OpenAI gateway service is not configured", nil)
	}
	if !account.IsExcelBPSEnabled() {
		return nil, nil, newUpstreamModelSyncConfigError("This BPS OAuth account cannot use Excel BPS (free plan or shadow account)", nil)
	}
	response, err := s.openaiGatewayService.FetchExcelBPSModelsList(ctx, account)
	if err != nil {
		slog.Warn("excel BPS upstream model sync failed", "account_id", account.ID, "error", err)
		message := "Failed to fetch Excel BPS model list"
		var appErr *infraerrors.ApplicationError
		if errors.As(err, &appErr) && strings.TrimSpace(appErr.Message) != "" {
			message += ": " + appErr.Message
		}
		return nil, nil, newUpstreamModelSyncUpstreamError(message, err)
	}
	models, err := extractUpstreamModelIDs(response.Body)
	if err != nil {
		return nil, nil, newUpstreamModelSyncUpstreamError("Excel BPS model list was not valid JSON", err)
	}
	if len(models) == 0 {
		return nil, nil, newUpstreamModelSyncUpstreamError("Excel BPS returned no supported models", nil)
	}
	return models, response.Body, nil
}
