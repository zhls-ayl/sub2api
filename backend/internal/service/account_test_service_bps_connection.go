package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// errExcelBPSNoTestModel reports a BPS-only account with no model to test.
var errExcelBPSNoTestModel = errors.New("no Excel BPS model is available for this account; select a model to test")

// testBPSOAuthAccountConnection routes every test mode of a BPS-only account
// through BPS: the tool round trip for bps_tools, otherwise a text probe. The
// model is the public (request-side) name; BPS forwarding maps it once.
func (s *AccountTestService) testBPSOAuthAccountConnection(c *gin.Context, account *Account, modelID, prompt, mode string) error {
	model := strings.TrimSpace(modelID)
	if model == "" {
		// The native test default is not a BPS model; use the account's first
		// configured model, as its test picker would.
		if models := ExcelBPSConfiguredModels(account); len(models) > 0 {
			model = models[0].ID
		}
	}
	if model == "" {
		return s.sendErrorAndEnd(c, errExcelBPSNoTestModel.Error())
	}
	if isOpenAIImageModel(account.GetMappedModel(model)) {
		return s.sendErrorAndEnd(c, "BPS OAuth accounts do not serve image models")
	}
	if s.openaiGatewayService == nil || validateExcelOAuthRoute(account, model) != nil {
		return s.sendErrorAndEnd(c, errExcelOAuthRouteUnavailable.Error())
	}
	if mode == AccountTestModeBPSTools {
		return s.testExcelBPSToolRoundtrip(c, account, model)
	}
	return s.testExcelBPSAccountConnection(c, account, model, prompt)
}

// testExcelBPSAccountConnection runs the default account test through the
// gateway Forward path, so a BPS-enabled account is probed on the same route
// user Responses requests take instead of the hard-coded native Codex probe.
func (s *AccountTestService) testExcelBPSAccountConnection(c *gin.Context, account *Account, modelID, prompt string) error {
	// Send the public name, like a user request: forwardExcelBPS maps it once.
	model := strings.TrimSpace(modelID)
	if model == "" {
		return s.sendErrorAndEnd(c, errExcelBPSNoTestModel.Error())
	}
	prompt = promptOrDefault(prompt)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})

	body, err := buildExcelBPSAccountTestBody(model, prompt)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Excel BPS test payload")
	}

	probe := httptest.NewRecorder()
	probeCtx, _ := gin.CreateTestContext(probe)
	probeCtx.Request = c.Request.Clone(s.withBPSTestEvidence(c.Request.Context(), c))
	if probeCtx.Request.Header == nil {
		probeCtx.Request.Header = make(http.Header)
	}
	// Manual one-shot tests have no client conversation. Give them a scoped
	// identity so enabling the session proxy does not break the test button.
	// Explicit identities (including load-test sessions) remain unchanged.
	if scope, _ := resolveOpenAIWSExecutionScope(probeCtx, body, 0); scope == "" {
		probeCtx.Request.Header.Set("Session-Id", "account-test-"+uuid.NewString())
	}
	result, err := s.openaiGatewayService.Forward(probeCtx.Request.Context(), probeCtx, account, body)
	if err != nil {
		// Name the BPS HTTP status so the failure is diagnosable from the test
		// dialog; the upstream_response evidence event carries the details.
		status := ""
		if code := probeCtx.GetInt(OpsUpstreamStatusCodeKey); code > 0 {
			status = fmt.Sprintf(" (HTTP %d)", code)
		}
		// A single-account test has no other account to fail over to.
		var failover *UpstreamFailoverError
		if errors.As(err, &failover) && failover.ClientMessage != "" {
			return s.sendErrorAndEnd(c, failover.ClientMessage+status)
		}
		var local *excelBPSForwardError
		if errors.As(err, &local) && strings.HasPrefix(local.code, "basispoints_auth_") {
			var payload struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(probe.Body.Bytes(), &payload) == nil && payload.Error.Message != "" {
				return s.sendErrorAndEnd(c, local.Error()+status+": "+payload.Error.Message)
			}
		}
		return s.sendErrorAndEnd(c, err.Error()+status)
	}

	answer := strings.Builder{}
	completed := false
	for _, line := range strings.Split(probe.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				_, _ = answer.WriteString(delta)
				s.sendEvent(c, TestEvent{Type: "content", Text: delta})
			}
		case "response.completed":
			completed = true
		}
	}
	if result == nil || result.ClientDisconnect {
		return s.sendErrorAndEnd(c, "Excel BPS test response was interrupted")
	}
	if !completed {
		return s.sendErrorAndEnd(c, "Excel BPS test response ended before completion")
	}
	if strings.TrimSpace(answer.String()) == "" {
		return s.sendErrorAndEnd(c, "Excel BPS returned empty output")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func buildExcelBPSAccountTestBody(model, prompt string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model, "stream": true, "store": false,
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": promptOrDefault(prompt)},
		}}},
		"reasoning": map[string]any{"effort": "medium"},
	})
}

func promptOrDefault(prompt string) string {
	if value := strings.TrimSpace(prompt); value != "" {
		return value
	}
	return "hi"
}
