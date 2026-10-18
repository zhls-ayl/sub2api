package leonardo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const uploadImageMutation = `mutation UploadImage($uploadImageInput: UploadImageInput!) {
  uploadImage(arg1: $uploadImageInput) {
    uploadId
    url
    fields
    __typename
  }
}`

const initImageModerationQuery = `query GetInitImageModeration($akUUID: uuid!) {
  init_image_moderation(where: {akUUID: {_eq: $akUUID}}) {
    akUUID
    initImageId
    checkStatus
    __typename
  }
}`

// UploadInitResult 是 UploadImage mutation 的响应。
type UploadInitResult struct {
	UploadID string `json:"uploadId"`
	Fields   string `json:"fields"`
	URL      string `json:"url"`
}

// UploadInitImage 初始化一个图片上传槽位（INIT + 扩展名），返回 S3 预签名
// POST 的 url 与表单字段（fields 是 JSON 字符串，需解出后随文件一起提交）。
func (c *Client) UploadInitImage(ctx context.Context, jwt, extension string) (*UploadInitResult, error) {
	extension = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(extension, ".")))
	if extension == "" {
		extension = "jpg"
	}
	body, err := c.doGraphQL(ctx, jwt, "UploadImage", map[string]any{
		"uploadImageInput": map[string]any{
			"uploadType": "INIT",
			"extension":  extension,
		},
	}, uploadImageMutation)
	if err != nil {
		return nil, err
	}
	if gqlErr := firstGraphQLError("UploadImage", body); gqlErr != nil {
		return nil, gqlErr
	}
	var gqlResp struct {
		Data struct {
			UploadImage UploadInitResult `json:"uploadImage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, NewRequestError(fmt.Sprintf("parse UploadImage response: %v", err))
	}
	result := gqlResp.Data.UploadImage
	if strings.TrimSpace(result.UploadID) == "" || strings.TrimSpace(result.URL) == "" {
		return nil, NewRequestError("UploadImage response missing uploadId or url")
	}
	return &result, nil
}

// PostToPresignedURL 把文件按预签名表单字段 multipart POST 到 S3。
func (c *Client) PostToPresignedURL(ctx context.Context, targetURL, fieldsJSON string, filename, contentType string, data []byte) error {
	var fields map[string]string
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return NewRequestError(fmt.Sprintf("parse upload fields: %v", err))
	}
	if len(fields) == 0 {
		return NewRequestError("upload fields were empty")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return &RequestError{Message: fmt.Sprintf("write upload field: %v", err), ErrorType: ErrorTypeNetwork}
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return &RequestError{Message: fmt.Sprintf("create upload file part: %v", err), ErrorType: ErrorTypeNetwork}
	}
	if _, err := part.Write(data); err != nil {
		return &RequestError{Message: fmt.Sprintf("write upload file part: %v", err), ErrorType: ErrorTypeNetwork}
	}
	if err := writer.Close(); err != nil {
		return &RequestError{Message: fmt.Sprintf("close upload writer: %v", err), ErrorType: ErrorTypeNetwork}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return &RequestError{Message: fmt.Sprintf("create presigned upload request: %v", err), ErrorType: ErrorTypeNetwork}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return wrapNetworkError("presigned upload failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("presigned upload returned %d", resp.StatusCode)
		if len(respBody) > 0 {
			message += ": " + strings.TrimSpace(string(respBody))
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests {
			return NewUpstreamTemporaryError(message, resp.StatusCode)
		}
		return NewRequestError(message)
	}
	return nil
}

// WaitForInitImage 轮询 init_image_moderation 直到上传的图片过审并产生
// initImageId（可直接用于 guidances.image_reference）。REJECTED / BLOCKED /
// FAILED / ERROR 视为内容拒绝。
func (c *Client) WaitForInitImage(ctx context.Context, jwt, uploadID string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(uploadID) == "" {
		return "", NewRequestError("upload id is required")
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	deadline := time.Now().Add(timeout)
	pollInterval := 1500 * time.Millisecond
	lastStatus := ""

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", ctx.Err()
		}
		body, err := c.doGraphQL(ctx, jwt, "GetInitImageModeration", map[string]any{"akUUID": uploadID}, initImageModerationQuery)
		if err != nil {
			if isRetryablePollError(err) {
				time.Sleep(pollInterval)
				continue
			}
			return "", err
		}
		if gqlErr := firstGraphQLError("GetInitImageModeration", body); gqlErr != nil {
			return "", gqlErr
		}
		var gqlResp struct {
			Data struct {
				InitImageModeration []struct {
					InitImageID string `json:"initImageId"`
					CheckStatus string `json:"checkStatus"`
				} `json:"init_image_moderation"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &gqlResp); err != nil {
			return "", NewRequestError(fmt.Sprintf("parse init image moderation response: %v", err))
		}
		if len(gqlResp.Data.InitImageModeration) > 0 {
			item := gqlResp.Data.InitImageModeration[0]
			lastStatus = strings.ToUpper(strings.TrimSpace(item.CheckStatus))
			if id := strings.TrimSpace(item.InitImageID); id != "" {
				return id, nil
			}
			switch lastStatus {
			case "FAILED", "REJECTED", "BLOCKED", "ERROR":
				return "", NewContentRejectedError("init image moderation " + strings.ToLower(lastStatus))
			}
		}
		time.Sleep(pollInterval)
	}
	if lastStatus != "" {
		return "", NewUpstreamTemporaryError(fmt.Sprintf("timed out waiting for init image id (last status: %s)", lastStatus), 0)
	}
	return "", NewUpstreamTemporaryError("timed out waiting for init image moderation", 0)
}

func isRetryablePollError(err error) bool {
	if _, ok := err.(*UpstreamTemporaryError); ok {
		return true
	}
	if requestErr, ok := err.(*RequestError); ok {
		return requestErr.StatusCode == http.StatusTooManyRequests
	}
	return false
}
