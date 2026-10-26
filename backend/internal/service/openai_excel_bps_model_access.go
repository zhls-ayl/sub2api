package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var excelBPSModelID = regexp.MustCompile(`^gpt-[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Convert only the official access response. Empty/denied access is authoritative;
// malformed envelopes and HTTP failures cannot manufacture a local allowlist.
func excelBPSAccessModelsBody(raw []byte) ([]byte, error) {
	var envelope struct {
		Allowed *bool `json:"allowed"`
		Catalog *struct {
			Models     *[]json.RawMessage `json:"models"`
			Restricted []string           `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Allowed == nil {
		return nil, fmt.Errorf("invalid Excel BPS access response")
	}
	models := make([]map[string]any, 0)
	if *envelope.Allowed {
		if envelope.Catalog == nil || envelope.Catalog.Models == nil {
			return nil, fmt.Errorf("missing Excel BPS model catalog")
		}
		restricted := map[string]bool{}
		for _, id := range envelope.Catalog.Restricted {
			restricted[id] = true
		}
		seen := map[string]bool{}
		for _, raw := range *envelope.Catalog.Models {
			var entry struct {
				ID        string `json:"id"`
				Label     string `json:"label"`
				Available *bool  `json:"available"`
				Enabled   *bool  `json:"enabled"`
				Picker    *bool  `json:"model_picker_enabled"`
				Policy    *struct {
					State string `json:"state"`
				} `json:"policy"`
				Efforts []struct {
					Value string `json:"value"`
				} `json:"efforts"`
			}
			if json.Unmarshal(raw, &entry) != nil || !excelBPSModelID.MatchString(entry.ID) || restricted[entry.ID] || seen[entry.ID] {
				continue
			}
			if entry.Available != nil && !*entry.Available || entry.Enabled != nil && !*entry.Enabled || entry.Picker != nil && !*entry.Picker || entry.Policy != nil && (entry.Policy.State == "disabled" || entry.Policy.State == "unconfigured") {
				continue
			}
			model := map[string]any{"id": entry.ID, "object": "model", "owned_by": "openai", "created": 0}
			if label := strings.TrimSpace(entry.Label); label != "" {
				model["display_name"] = label
			}
			efforts := make([]string, 0)
			effortSeen := map[string]bool{}
			for _, effort := range entry.Efforts {
				switch effort.Value {
				case "low", "medium", "high", "xhigh":
					if !effortSeen[effort.Value] {
						efforts = append(efforts, effort.Value)
						effortSeen[effort.Value] = true
					}
				}
			}
			if entry.Efforts != nil {
				model["reasoning_efforts"] = efforts
				model["supported_reasoning_levels"] = efforts
			}
			models = append(models, model)
			seen[entry.ID] = true
		}
	}
	return json.Marshal(map[string]any{"object": "list", "data": models})
}

// FetchExcelBPSModelsList reads the models BPS currently offers this account
// (GET /basispoints/api/responses/access?include_models=true). Only "sync
// upstream models" uses it, on an explicit admin action, so it is not cached.
func (s *OpenAIGatewayService) FetchExcelBPSModelsList(ctx context.Context, account *Account) (*OpenAIModelsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.httpUpstream == nil || account == nil || !account.IsOpenAI() || !account.IsExcelBPSEnabled() {
		return nil, fmt.Errorf("excel BPS model discovery is unavailable")
	}
	token, err := s.getExcelBPSAccessToken(ctx, account)
	if err != nil {
		return nil, infraerrors.New(502, "EXCEL_BPS_MODELS_AUTH_UNAVAILABLE", "Excel BPS credentials are unavailable")
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return nil, fmt.Errorf("excel BPS model discovery requires an account ID")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, excelBPSModelsFetchTimeout)
	defer cancel()
	request, err := newExcelBPSRequest(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(fetchCtx, HTTPUpstreamProfileExcelBPS)), nil, token, accountID)
	if err != nil {
		return nil, err
	}
	request.Method = http.MethodGet
	request.Body = nil
	request.ContentLength = 0
	request.Header.Set("Accept", "application/json")
	request.URL.Path = "/basispoints/api/responses/access"
	request.URL.RawQuery = "include_models=true"
	response, err := s.httpUpstream.Do(request, upstreamModelsProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, infraerrors.New(502, "EXCEL_BPS_MODELS_UPSTREAM_FAILED", "Excel BPS model discovery request failed")
	}
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("excel BPS model discovery returned no response")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, infraerrors.New(502, "EXCEL_BPS_MODELS_UPSTREAM_REJECTED", fmt.Sprintf("Excel BPS model discovery returned HTTP %d", response.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, openAIModelsCacheBodyLimit+1))
	if err != nil || len(raw) > openAIModelsCacheBodyLimit {
		return nil, fmt.Errorf("excel BPS model catalog could not be read")
	}
	body, err := excelBPSAccessModelsBody(raw)
	if err != nil {
		return nil, err
	}
	return &OpenAIModelsResponse{Body: body, ETag: codexModelsManifestBodyETag(body)}, nil
}

const excelBPSModelsFetchTimeout = 5 * time.Second

// excelBPSAccountModelsResponse is a BPS-only account's raw model catalog,
// built from its configuration (see excelBPSUpstreamModelIDs).
func excelBPSAccountModelsResponse(account *Account) *OpenAIModelsResponse {
	body := excelBPSConfiguredModelsBody(account)
	return &OpenAIModelsResponse{Body: body, ETag: codexModelsManifestBodyETag(body)}
}

// excelBPSCodexManifest is a BPS-only account's Codex manifest, built locally
// from its configured models. BPS cannot carry multi-agent v2 state, so those
// fields are cleared on every model.
func excelBPSCodexManifest(account *Account, ifNoneMatch string) (*OpenAIModelsResponse, error) {
	body := convertOpenAIModelListToCodexManifestForAccount(excelBPSConfiguredModelsBody(account), account)
	body, _, err := clearCodexManifestMultiAgent(body, func(string) bool { return true })
	if err != nil {
		return nil, err
	}
	manifest := &OpenAIModelsResponse{Body: body, ETag: codexModelsManifestBodyETag(body)}
	return openAIModelsResponseForClient(manifest, ifNoneMatch), nil
}
