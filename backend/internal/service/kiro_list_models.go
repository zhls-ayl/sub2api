package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	kiropkg "github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/google/uuid"
)

const (
	kiroListModelsMaxPages       = 10
	kiroListModelsPageSize       = "50"
	kiroListModelsFallbackRegion = "eu-central-1"
	kiroListModelsOrigin         = "AI_EDITOR"
)

type kiroModelsDoFunc func(req *http.Request) (*http.Response, error)

// kiroModelTokenLimits 是 ListAvailableModels 里每个模型的 tokenLimits。
// 缺字段或非正数表示未知，请求时不按猜测值拒绝。
type kiroModelTokenLimits struct {
	MaxInputTokens  int64 `json:"maxInputTokens"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
}

// kiroListedModel 是一次模型列表同步里保留下来的上游模型及其 token 上限。
type kiroListedModel struct {
	ID              string
	MaxInputTokens  int64
	MaxOutputTokens int64
}

type kiroListAvailableModel struct {
	ModelID     string               `json:"modelId"`
	TokenLimits kiroModelTokenLimits `json:"tokenLimits"`
}

type kiroListAvailableModelsResponse struct {
	Models    []kiroListAvailableModel `json:"models"`
	NextToken string                   `json:"nextToken"`
}

func kiroListedModelFrom(id string, limits kiroModelTokenLimits) (kiroListedModel, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return kiroListedModel{}, false
	}
	if limits.MaxInputTokens < 0 {
		limits.MaxInputTokens = 0
	}
	if limits.MaxOutputTokens < 0 {
		limits.MaxOutputTokens = 0
	}
	return kiroListedModel{
		ID:              id,
		MaxInputTokens:  limits.MaxInputTokens,
		MaxOutputTokens: limits.MaxOutputTokens,
	}, true
}

func kiroListedModelIDs(models []kiroListedModel) []string {
	if len(models) == 0 {
		return nil
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// kiroListAvailableModelIDs 调用 GET https://q.{region}.amazonaws.com/ListAvailableModels。
// 账号区域返回 403 时，us-east-1 再试 eu-central-1，eu-central-1 再试 us-east-1。最多翻 10 页。
func kiroListAvailableModelIDs(ctx context.Context, account *Account, token string) ([]string, error) {
	models, err := kiroListAvailableModels(ctx, account, token)
	if err != nil {
		return nil, err
	}
	return kiroListedModelIDs(models), nil
}

func kiroListAvailableModels(ctx context.Context, account *Account, token string) ([]kiroListedModel, error) {
	return kiroListAvailableModelsWithDo(ctx, account, token, kiroModelsHTTPDo(account))
}

func kiroModelsHTTPDo(account *Account) kiroModelsDoFunc {
	return func(req *http.Request) (*http.Response, error) {
		client, err := httpclient.GetClient(httpclient.Options{
			ProxyURL:           kiroProxyURL(account),
			Timeout:            30 * time.Second,
			ValidateResolvedIP: true,
		})
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}
}

func kiroListAvailableModelIDsWithDo(ctx context.Context, account *Account, token string, do kiroModelsDoFunc) ([]string, error) {
	models, err := kiroListAvailableModelsWithDo(ctx, account, token, do)
	if err != nil {
		return nil, err
	}
	return kiroListedModelIDs(models), nil
}

func kiroListAvailableModelsWithDo(ctx context.Context, account *Account, token string, do kiroModelsDoFunc) ([]kiroListedModel, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("kiro access token is empty")
	}
	if do == nil {
		return nil, fmt.Errorf("kiro models transport is nil")
	}

	region := kiroAPIRegion(account)
	models, status, err := kiroListModelsPages(ctx, account, token, region, do)
	if status == http.StatusForbidden {
		// us-east-1 → eu-central-1 matches upstream. Enterprise accounts whose
		// api_region is already eu-central-1 still need the reverse hop.
		fallbackRegion := kiroListModelsFallbackRegion
		if strings.EqualFold(region, kiroListModelsFallbackRegion) {
			fallbackRegion = kiroDefaultRegion
		}
		if !strings.EqualFold(region, fallbackRegion) {
			fallbackModels, _, fallbackErr := kiroListModelsPages(ctx, account, token, fallbackRegion, do)
			if fallbackErr == nil {
				return fallbackModels, nil
			}
			return nil, fallbackErr
		}
	}
	return models, err
}

func kiroListModelsPages(ctx context.Context, account *Account, token, region string, do kiroModelsDoFunc) ([]kiroListedModel, int, error) {
	seen := make(map[string]struct{})
	var models []kiroListedModel
	nextToken := ""
	for page := 0; page < kiroListModelsMaxPages; page++ {
		req, err := buildKiroListModelsRequest(ctx, account, token, region, nextToken)
		if err != nil {
			return nil, 0, err
		}
		resp, err := do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("list available models request: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, resp.StatusCode, fmt.Errorf("read list available models response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, resp.StatusCode, fmt.Errorf("list available models: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var parsed kiroListAvailableModelsResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("decode list available models response: %w", err)
		}
		for _, model := range parsed.Models {
			listed, ok := kiroListedModelFrom(model.ModelID, model.TokenLimits)
			if !ok {
				continue
			}
			if _, exists := seen[listed.ID]; exists {
				continue
			}
			seen[listed.ID] = struct{}{}
			models = append(models, listed)
		}
		nextToken = strings.TrimSpace(parsed.NextToken)
		if nextToken == "" {
			return models, http.StatusOK, nil
		}
	}
	return models, http.StatusOK, nil
}

func buildKiroListModelsRequest(ctx context.Context, account *Account, token, region, nextToken string) (*http.Request, error) {
	query := url.Values{}
	query.Set("origin", kiroListModelsOrigin)
	query.Set("maxResults", kiroListModelsPageSize)
	if profileArn := strings.TrimSpace(kiroResolveRequestProfileArn(account)); profileArn != "" {
		query.Set("profileArn", profileArn)
	}
	if tokenValue := strings.TrimSpace(nextToken); tokenValue != "" {
		query.Set("nextToken", tokenValue)
	}
	endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/ListAvailableModels?%s", region, query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create list models request: %w", err)
	}
	accountKey := buildKiroAccountKey(account)
	machineID := buildKiroMachineID(account)
	userAgent := kiropkg.BuildRuntimeUserAgent(accountKey, machineID)
	amzUserAgent := kiropkg.BuildRuntimeAmzUserAgent(accountKey, machineID)
	sdkRequest := "attempt=1; max=3"
	oauthList := account != nil && account.Type == AccountTypeOAuth
	if oauthList {
		userAgent = kiropkg.BuildUsageRuntimeUserAgent(accountKey, machineID)
		amzUserAgent = kiropkg.BuildUsageRuntimeAmzUserAgent(accountKey, machineID)
		sdkRequest = "attempt=1; max=1"
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Amz-User-Agent", amzUserAgent)
	if oauthList {
		req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	}
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("Amz-Sdk-Request", sdkRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	applyKiroConditionalHeaders(req, account)
	return req, nil
}

func (s *AccountTestService) fetchKiroDirectUpstreamModels(ctx context.Context, account *Account) ([]string, error) {
	token, err := s.kiroSyncAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	var repo AccountRepository
	if s != nil {
		repo = s.accountRepo
	}
	ensureKiroEnterpriseRealProfileArn(ctx, repo, account, token)
	listed, err := kiroListAvailableModels(ctx, account, token)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to fetch Kiro available models", err)
	}
	names, _ := kiroPublicCatalogFromListed(listed)
	if len(names) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	return names, nil
}

func (s *AccountTestService) syncKiroDirectModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	token, err := s.kiroSyncAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	var repo AccountRepository
	if s != nil {
		repo = s.accountRepo
	}
	ensureKiroEnterpriseRealProfileArn(ctx, repo, account, token)
	listed, err := kiroListAvailableModels(ctx, account, token)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to fetch Kiro available models", err)
	}
	return s.kiroCatalogFromListed(ctx, account, listed)
}

// kiroCatalogFromListed 展开对外模型名，并把 tokenLimits 写进账号的 upstream_model_metadata。
func (s *AccountTestService) kiroCatalogFromListed(ctx context.Context, account *Account, listed []kiroListedModel) (*UpstreamModelCatalog, error) {
	names, metadata := kiroPublicCatalogFromListed(listed)
	if len(names) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	if err := s.persistKiroModelTokenLimits(ctx, account, metadata); err != nil {
		return nil, err
	}
	return &UpstreamModelCatalog{Models: names, Metadata: metadata}, nil
}

// persistKiroModelTokenLimits 把带上下文窗口或输出上限的模型写入账号 extra。
// 这次同步没有任何 tokenLimits 时保留已有快照，避免一次空响应清掉已知上限。
func (s *AccountTestService) persistKiroModelTokenLimits(ctx context.Context, account *Account, metadata map[string]UpstreamModelMetadata) error {
	useful := kiroTokenLimitMetadata(metadata)
	if len(useful) == 0 || s == nil || account == nil || account.ID <= 0 || s.accountRepo == nil {
		return nil
	}
	snapshot := UpstreamModelMetadataSnapshot{
		Source:   "kiro",
		SyncedAt: time.Now().UTC().Format(time.RFC3339),
		Models:   useful,
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{UpstreamModelMetadataExtraKey: snapshot}); err != nil {
		return newUpstreamModelSyncInternalError("Failed to save upstream model metadata", err)
	}
	account.SetUpstreamModelMetadataSnapshot(snapshot)
	return nil
}

func kiroTokenLimitMetadata(metadata map[string]UpstreamModelMetadata) map[string]UpstreamModelMetadata {
	if len(metadata) == 0 {
		return nil
	}
	useful := make(map[string]UpstreamModelMetadata, len(metadata))
	for name, meta := range metadata {
		if meta.ContextWindow <= 0 && meta.MaxContextWindow <= 0 && meta.MaxOutputTokens <= 0 {
			continue
		}
		useful[name] = meta
	}
	if len(useful) == 0 {
		return nil
	}
	return useful
}

func (s *AccountTestService) kiroSyncAccessToken(ctx context.Context, account *Account) (string, error) {
	if account != nil && account.Type == AccountTypeAPIKey {
		token := firstKiroCredential(account, "kiro_api_key", "kiroApiKey", "api_key")
		if token == "" {
			return "", newUpstreamModelSyncConfigError("No Kiro API key is available", nil)
		}
		return token, nil
	}
	if s == nil || s.kiroTokenProvider == nil {
		return "", newUpstreamModelSyncConfigError("Kiro token provider is not configured", nil)
	}
	token, err := s.kiroTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return "", newUpstreamModelSyncUpstreamError("Failed to get Kiro access token", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", newUpstreamModelSyncConfigError("No Kiro access token is available", nil)
	}
	return token, nil
}

// kiroSyncPublicModelNames 把上游 modelId 展开成对外名。Claude 紧跟一行 -thinking。
func kiroSyncPublicModelNames(ids []string) []string {
	names, _ := kiroPublicCatalog(ids)
	return names
}

// kiroPublicCatalog 展开对外名，并在 metadata 里记下原始 modelId。
// thinking 别名指向同一个上游 ID。界面用这个 ID 保存映射，不再从短横线名字猜点号位置。
func kiroPublicCatalog(ids []string) ([]string, map[string]UpstreamModelMetadata) {
	models := make([]kiroListedModel, 0, len(ids))
	for _, id := range ids {
		listed, ok := kiroListedModelFrom(id, kiroModelTokenLimits{})
		if !ok {
			continue
		}
		models = append(models, listed)
	}
	return kiroPublicCatalogFromListed(models)
}

// kiroPublicCatalogFromListed 展开对外名，并把 tokenLimits 抄到每个别名上。
// 点号形式的上游 ID 也写入 metadata，方便按请求模型或映射目标查找，不进入对外模型列表。
func kiroPublicCatalogFromListed(models []kiroListedModel) ([]string, map[string]UpstreamModelMetadata) {
	seen := make(map[string]struct{})
	names := make([]string, 0, len(models))
	metadata := make(map[string]UpstreamModelMetadata)
	for _, model := range models {
		upstreamID := strings.TrimSpace(model.ID)
		for _, pair := range kiropkg.SyncModelAliases(upstreamID) {
			name := pair[0]
			if name == "" {
				continue
			}
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				names = append(names, name)
			}
			if _, ok := metadata[name]; ok {
				continue
			}
			metadata[name] = kiroUpstreamMetadata(pair[1], model)
		}
		if upstreamID == "" {
			continue
		}
		if _, ok := metadata[upstreamID]; ok {
			continue
		}
		metadata[upstreamID] = kiroUpstreamMetadata(upstreamID, model)
	}
	return names, metadata
}

func kiroUpstreamMetadata(upstreamID string, model kiroListedModel) UpstreamModelMetadata {
	meta := UpstreamModelMetadata{ID: upstreamID}
	if model.MaxInputTokens > 0 {
		meta.ContextWindow = model.MaxInputTokens
		meta.MaxContextWindow = model.MaxInputTokens
	}
	if model.MaxOutputTokens > 0 {
		meta.MaxOutputTokens = model.MaxOutputTokens
	}
	return meta
}

// kiroStoredMaxInputTokens 返回账号已同步的 maxInputTokens。
// 列表没拉过、或模型不在列表里时返回 0，调用方保持原有行为，不按猜测值拒绝。
func kiroStoredMaxInputTokens(account *Account, models ...string) int64 {
	if account == nil {
		return 0
	}
	for _, key := range kiroModelMetadataKeys(models...) {
		meta, ok := account.GetUpstreamModelMetadata(key)
		if !ok {
			continue
		}
		if meta.ContextWindow > 0 {
			return meta.ContextWindow
		}
		if meta.MaxContextWindow > 0 {
			return meta.MaxContextWindow
		}
	}
	return 0
}

// kiroStoredMaxOutputTokens 返回账号已同步的 maxOutputTokens。
// 没有记录时返回 0，调用方不改请求里的 max_tokens。
func kiroStoredMaxOutputTokens(account *Account, models ...string) int64 {
	if account == nil {
		return 0
	}
	for _, key := range kiroModelMetadataKeys(models...) {
		meta, ok := account.GetUpstreamModelMetadata(key)
		if !ok {
			continue
		}
		if meta.MaxOutputTokens > 0 {
			return meta.MaxOutputTokens
		}
	}
	return 0
}

func kiroModelMetadataKeys(models ...string) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0, len(models)*4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		keys = append(keys, value)
		lower := strings.ToLower(value)
		if _, ok := seen[lower]; ok {
			return
		}
		seen[lower] = struct{}{}
		keys = append(keys, lower)
	}
	for _, model := range models {
		add(model)
		if publicID := kiropkg.PublicModelID(model); publicID != "" {
			add(publicID)
		}
		if mapped := kiropkg.MapModel(model); mapped != "" {
			add(mapped)
			add(kiropkg.PublicModelID(mapped))
		}
	}
	return keys
}
