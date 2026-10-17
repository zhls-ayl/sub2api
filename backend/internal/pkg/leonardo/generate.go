package leonardo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const generateMutation = `mutation Generate($request: CreateGenerationRequest!) {
  generate(request: $request) {
    apiCreditCost
    generationId
    cost {
      amount
      unit
      __typename
    }
    __typename
  }
}`

const statusQuery = `query GetAIGenerationFeedStatuses($where: generations_bool_exp = {}) {
  generations(where: $where) {
    id
    status
    __typename
  }
}`

const generationDetailQuery = `query GetGenerationDetail($where: generations_bool_exp = {}) {
  generations(where: $where) {
    id
    status
    prompt
    modelId
    imageWidth
    imageHeight
    createdAt
    generated_images(order_by: [{url: desc}]) {
      id
      url
      __typename
    }
    __typename
  }
}`

// 生成状态枚举。
const (
	StatusPending  = "PENDING"
	StatusRunning  = "RUNNING"
	StatusComplete = "COMPLETE"
	StatusFailed   = "FAILED"
	StatusUnknown  = "UNKNOWN"
)

// IsCompleteStatus 报告状态是否为完成（COMPLETED 与 COMPLETE 同义，leo_proxy 生产见过）。
func IsCompleteStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case StatusComplete, "COMPLETED":
		return true
	default:
		return false
	}
}

// IsFailedStatus 报告状态是否为失败终态（ERROR 与 FAILED 同义）。
func IsFailedStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case StatusFailed, "ERROR":
		return true
	default:
		return false
	}
}

// GenerateRequest 是 Generate mutation 的 request 变量。
type GenerateRequest struct {
	Model      string         `json:"model"`
	Public     bool           `json:"public"`
	Parameters GenerateParams `json:"parameters"`
}

// GenerateParams 覆盖 gpt-image-2 族用到的参数字段。
type GenerateParams struct {
	Width         int      `json:"width"`
	Height        int      `json:"height"`
	Prompt        string   `json:"prompt"`
	Quantity      int      `json:"quantity"`
	Quality       string   `json:"quality,omitempty"`
	PromptEnhance string   `json:"prompt_enhance,omitempty"`
	StyleIDs      []string `json:"style_ids,omitempty"`
	// Transparency 为 true 时出透明背景 PNG（generations.transparency =
	// foreground_only）；false 时不带该字段。
	Transparency bool               `json:"transparency,omitempty"`
	Guidances    *GenerateGuidances `json:"guidances,omitempty"`
}

// GenerateGuidances 是参考图引导（image-to-image / edits）。
type GenerateGuidances struct {
	ImageReference []ImageReferenceGuidance `json:"image_reference,omitempty"`
}

// ImageReferenceGuidance 引用一个已上传并通过审核的 init image。
type ImageReferenceGuidance struct {
	Image struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"image"`
	Strength string `json:"strength"`
}

// GuidanceUploadedType 是 guidance 里 image.type 的固定值。
const GuidanceUploadedType = "UPLOADED"

// GuidanceStrengthMID 是 gpt-image-2 参考图的固定强度。
const GuidanceStrengthMID = "MID"

// GenerateResult 是提交成功的响应。
type GenerateResult struct {
	GenerationID string `json:"generationId"`
	// APICreditCost 是上游确认的积分消耗。gpt-image-2.5 系列的 apiCreditCost
	// 返回 null，实耗在 cost.amount（字符串）里；解析时回落到它。
	APICreditCost int `json:"apiCreditCost"`
	// CostUnit 是 cost.unit（如 CREDITS），未返回时为空。
	CostUnit string `json:"-"`
}

// generateCost 是 Generate 响应的 cost 字段；amount 上游给字符串，兼容数字。
type generateCost struct {
	Amount json.RawMessage `json:"amount"`
	Unit   string          `json:"unit"`
}

func (c *generateCost) amount() (int, bool) {
	if c == nil || len(c.Amount) == 0 {
		return 0, false
	}
	raw := strings.Trim(strings.TrimSpace(string(c.Amount)), `"`)
	if raw == "" || raw == "null" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return int(math.Round(value)), true
}

// GenerationStatus 是轮询到的状态。
type GenerationStatus struct {
	ID     string
	Status string
}

// GenerationImage 是生成产物。
type GenerationImage struct {
	ID  string
	URL string
}

// GenerationDetail 是完成后的生成详情。
type GenerationDetail struct {
	ID          string
	Status      string
	ImageWidth  int
	ImageHeight int
	Images      []GenerationImage
}

// SubmitGeneration 提交一次生成，返回 generationId 与上游确认的积分消耗。
// 调用方注意：网络类错误（RequestError.IsNetwork）意味着提交结果不明——
// 上游可能已受理并扣积分，不得自动重提。
func (c *Client) SubmitGeneration(ctx context.Context, jwt string, request GenerateRequest) (*GenerateResult, error) {
	body, err := c.doGraphQL(ctx, jwt, "Generate", map[string]any{"request": request}, generateMutation)
	if err != nil {
		return nil, err
	}
	if gqlErr := firstGraphQLError("Generate", body); gqlErr != nil {
		return nil, gqlErr
	}
	var gqlResp struct {
		Data struct {
			Generate struct {
				GenerationID  string        `json:"generationId"`
				APICreditCost *int          `json:"apiCreditCost"`
				Cost          *generateCost `json:"cost"`
			} `json:"generate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse Generate response: %v", err))
	}
	generated := gqlResp.Data.Generate
	if strings.TrimSpace(generated.GenerationID) == "" {
		return nil, NewRequestError("Generate response missing generationId")
	}
	result := &GenerateResult{GenerationID: generated.GenerationID}
	if generated.APICreditCost != nil {
		result.APICreditCost = *generated.APICreditCost
	} else if amount, ok := generated.Cost.amount(); ok {
		result.APICreditCost = amount
	}
	if generated.Cost != nil {
		result.CostUnit = strings.TrimSpace(generated.Cost.Unit)
	}
	return result, nil
}

// PollGenerationStatus 查询生成状态。查询按 id 过滤（不按 status 过滤，
// 避免 where 枚举与上游状态集漂移）；查不到返回 UNKNOWN。
func (c *Client) PollGenerationStatus(ctx context.Context, jwt, generationID string) (*GenerationStatus, error) {
	body, err := c.doGraphQL(ctx, jwt, "GetAIGenerationFeedStatuses", map[string]any{
		"where": map[string]any{"id": map[string]any{"_eq": generationID}},
	}, statusQuery)
	if err != nil {
		return nil, err
	}
	if gqlErr := firstGraphQLError("GetAIGenerationFeedStatuses", body); gqlErr != nil {
		return nil, gqlErr
	}
	var gqlResp struct {
		Data struct {
			Generations []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"generations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse status response: %v", err))
	}
	for _, gen := range gqlResp.Data.Generations {
		if strings.EqualFold(strings.TrimSpace(gen.ID), strings.TrimSpace(generationID)) {
			return &GenerationStatus{ID: gen.ID, Status: strings.ToUpper(strings.TrimSpace(gen.Status))}, nil
		}
	}
	return &GenerationStatus{ID: generationID, Status: StatusUnknown}, nil
}

// GetGenerationDetail 拉取生成详情（完成后取产物 URL；图片输出取
// generated_images[].url，注意该 CDN URL 有过期时间，需立即下载）。
func (c *Client) GetGenerationDetail(ctx context.Context, jwt, generationID string) (*GenerationDetail, error) {
	body, err := c.doGraphQL(ctx, jwt, "GetGenerationDetail", map[string]any{
		"where": map[string]any{"id": map[string]any{"_eq": generationID}},
	}, generationDetailQuery)
	if err != nil {
		return nil, err
	}
	if gqlErr := firstGraphQLError("GetGenerationDetail", body); gqlErr != nil {
		return nil, gqlErr
	}
	var gqlResp struct {
		Data struct {
			Generations []struct {
				ID              string `json:"id"`
				Status          string `json:"status"`
				ImageWidth      int    `json:"imageWidth"`
				ImageHeight     int    `json:"imageHeight"`
				GeneratedImages []struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"generated_images"`
			} `json:"generations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse detail response: %v", err))
	}
	if len(gqlResp.Data.Generations) == 0 {
		return nil, NewRequestError(fmt.Sprintf("generation %s not found", generationID))
	}
	gen := gqlResp.Data.Generations[0]
	detail := &GenerationDetail{
		ID:          gen.ID,
		Status:      strings.ToUpper(strings.TrimSpace(gen.Status)),
		ImageWidth:  gen.ImageWidth,
		ImageHeight: gen.ImageHeight,
	}
	for _, img := range gen.GeneratedImages {
		detail.Images = append(detail.Images, GenerationImage{ID: img.ID, URL: img.URL})
	}
	return detail, nil
}

// 失败原因探测。当前 schema 里 notes.failureReason 是 JSON 值（如
// {"errorCode":"PROVIDER_MODERATION_ERROR","clientSummary":"…"}），先按 JSON
// 值查；旧 schema 是对象类型（只能按子字段查），作为第二步兜底。再不命中则
// 用 noteType（如 CC_NSFW_TOTAL_FAILURE，failureReason 为 null）与 nsfw/flagged
// 兜底。探测失败不影响主流程（返回 generic 原因）。
const failureNotesScalarQuery = `query GetGenerationFailureNotes($id: uuid!) {
  generations(where: {id: {_eq: $id}}) {
    nsfw
    flagged
    notes {
      noteType
      failureReason
    }
  }
}`

const failureNotesObjectQuery = `query GetGenerationFailureNotes($id: uuid!) {
  generations(where: {id: {_eq: $id}}) {
    nsfw
    flagged
    notes {
      noteType
      failureReason {
        errorCode
      }
    }
  }
}`

// failureNoteTypeCodes 是 failureReason 为空时可直接当错误码用的 noteType。
var failureNoteTypeCodes = map[string]bool{
	nsfwNoteTypeCode: true,
}

const nsfwNoteTypeCode = "CC_NSFW_TOTAL_FAILURE"

// GenerationFailure 是失败原因的归一化结果。
type GenerationFailure struct {
	ErrorCode string
	// NoteType 是命中的 generation_notes.noteType（如 PROVIDER_FAILURE）。
	NoteType string
	// ClientSummary 是上游给终端用户看的失败说明（failureReason.clientSummary）。
	ClientSummary string
	NSFW          bool
	Flagged       bool
}

// FailureReason 为 FAILED 生成拉取失败原因，归类规则见 ClassifyFailureErrorCode。
func (c *Client) FailureReason(ctx context.Context, jwt, generationID string) (*GenerationFailure, error) {
	for _, query := range []string{failureNotesScalarQuery, failureNotesObjectQuery} {
		failure, matched, err := c.probeFailureNotes(ctx, jwt, generationID, query)
		if err != nil {
			return nil, err
		}
		if matched {
			return failure, nil
		}
	}
	return &GenerationFailure{ErrorCode: ""}, nil
}

func (c *Client) probeFailureNotes(ctx context.Context, jwt, generationID, query string) (*GenerationFailure, bool, error) {
	body, err := c.doGraphQL(ctx, jwt, "GetGenerationFailureNotes", map[string]any{"id": generationID}, query)
	if err != nil {
		// schema 版本不含该字段形态（校验失败）或瞬时故障：交由调用方探测
		// 下一种形态 / 回退 generic 原因。封号等信号在提交与轮询阶段已暴露。
		return nil, false, nil
	}
	if gqlErr := firstGraphQLError("GetGenerationFailureNotes", body); gqlErr != nil {
		return nil, false, nil
	}
	var gqlResp struct {
		Data struct {
			Generations []struct {
				NSFW    bool `json:"nsfw"`
				Flagged bool `json:"flagged"`
				Notes   []struct {
					NoteType      string          `json:"noteType"`
					FailureReason json.RawMessage `json:"failureReason"`
				} `json:"notes"`
			} `json:"generations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, false, nil
	}
	if len(gqlResp.Data.Generations) == 0 {
		return nil, false, nil
	}
	gen := gqlResp.Data.Generations[0]
	failure := &GenerationFailure{NSFW: gen.NSFW, Flagged: gen.Flagged}
	// 优先取带 failureReason 的 note；都没有时再看 noteType 是否本身就是错误码。
	for _, note := range gen.Notes {
		raw := strings.TrimSpace(string(note.FailureReason))
		if raw == "" || raw == "null" {
			continue
		}
		var objectForm struct {
			ErrorCode     string `json:"errorCode"`
			ClientSummary string `json:"clientSummary"`
		}
		if err := json.Unmarshal(note.FailureReason, &objectForm); err == nil && objectForm.ErrorCode != "" {
			failure.ErrorCode = strings.TrimSpace(objectForm.ErrorCode)
			failure.ClientSummary = strings.TrimSpace(objectForm.ClientSummary)
			failure.NoteType = strings.TrimSpace(note.NoteType)
			return failure, true, nil
		}
		var scalarForm string
		if err := json.Unmarshal(note.FailureReason, &scalarForm); err == nil && scalarForm != "" {
			failure.ErrorCode = strings.TrimSpace(scalarForm)
			failure.NoteType = strings.TrimSpace(note.NoteType)
			return failure, true, nil
		}
	}
	for _, note := range gen.Notes {
		noteType := strings.ToUpper(strings.TrimSpace(note.NoteType))
		if failureNoteTypeCodes[noteType] {
			failure.ErrorCode = noteType
			failure.NoteType = noteType
			return failure, true, nil
		}
	}
	// 字段形态匹配（无 schema 错误）但没有可用的错误码：nsfw/flagged
	// 兜底也随本形态返回。
	return failure, true, nil
}

// ClassifyFailureErrorCode 把失败原因映射为错误类型：
//   - PROVIDER_MODERATION_ERROR → ContentRejectedError（ProviderModeration：上游
//     模型方审核，结果不稳定，调用方可重试一次）；
//   - CC_NSFW_TOTAL_FAILURE / nsfw / flagged → ContentRejectedError（Leonardo 自身
//     拦截，确定性的）；
//   - PROVIDER_AUTHENTICATION_ERROR → AuthError；
//   - PROVIDER_RATE_LIMIT / INTERNAL_ERROR / TIMEOUT / OUTPUT_ERROR /
//     ALL_PROVIDERS_FAILED → UpstreamTemporaryError；其余 → RequestError。
//
// 上游给了 clientSummary 时作为面向用户的文案（UserMessage），内部 Message
// 保留错误码便于日志检索。
func ClassifyFailureErrorCode(failure *GenerationFailure) error {
	code := strings.ToUpper(strings.TrimSpace(failure.ErrorCode))
	message := "leonardo generation failed"
	if code != "" {
		message = "leonardo generation failed: " + code
	}
	if summary := strings.TrimSpace(failure.ClientSummary); summary != "" {
		message += " (" + summary + ")"
	}
	withSummary := func(reqErr *RequestError) {
		reqErr.UserMessage = strings.TrimSpace(failure.ClientSummary)
	}
	switch code {
	case "PROVIDER_MODERATION_ERROR":
		rejected := NewContentRejectedError(message)
		rejected.ProviderModeration = true
		withSummary(&rejected.RequestError)
		return rejected
	case nsfwNoteTypeCode:
		rejected := NewContentRejectedError(message)
		withSummary(&rejected.RequestError)
		return rejected
	case "PROVIDER_AUTHENTICATION_ERROR":
		return NewAuthError(message, 0)
	case "PROVIDER_RATE_LIMIT", "PROVIDER_INTERNAL_ERROR", "PROVIDER_TIMEOUT", "PROVIDER_OUTPUT_ERROR", "ALL_PROVIDERS_FAILED":
		return NewUpstreamTemporaryError(message, 0)
	}
	if failure.NSFW || failure.Flagged {
		rejected := NewContentRejectedError(message)
		withSummary(&rejected.RequestError)
		return rejected
	}
	reqErr := NewRequestError(message)
	withSummary(reqErr)
	return reqErr
}
