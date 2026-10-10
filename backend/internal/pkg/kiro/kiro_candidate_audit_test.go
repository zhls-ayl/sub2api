package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCandidateRetainedPairsAreComplete(t *testing.T) {
	built, err := BuildKiroPayloadWithContext(
		makeLongToolConversation(), "claude-sonnet-4.5", "", "AI_EDITOR", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Payload) > kiroMaxPayloadBytes {
		t.Fatal("payload exceeds limit")
	}

	pending := map[string]bool{}
	consume := func(results []gjson.Result) {
		for _, result := range results {
			id := result.Get("toolUseId").String()
			if !pending[id] {
				t.Fatalf("unknown or duplicate result: %s", id)
			}
			delete(pending, id)
		}
		if len(pending) != 0 {
			t.Fatal("retained tool calls lack results")
		}
	}
	for _, entry := range gjson.GetBytes(built.Payload, "conversationState.history").Array() {
		if assistant := entry.Get("assistantResponseMessage"); assistant.Exists() {
			if len(pending) != 0 {
				t.Fatal("unresolved calls before next assistant")
			}
			for _, tool := range assistant.Get("toolUses").Array() {
				id := tool.Get("toolUseId").String()
				if id == "" || pending[id] {
					t.Fatal("empty or duplicate call ID")
				}
				pending[id] = true
			}
		} else if user := entry.Get("userInputMessage"); user.Exists() {
			consume(user.Get("userInputMessageContext.toolResults").Array())
		}
	}
	results := gjson.GetBytes(built.Payload,
		"conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults").Array()
	if len(results) != 1 || results[0].Get("toolUseId").String() != "toolu_0011" {
		t.Fatal("latest tool result changed or disappeared")
	}
	consume(results)
}

func TestCandidateOversizedToolInputPreserved(t *testing.T) {
	assistant := &KiroAssistantResponseMessage{
		ToolUses: []KiroToolUse{{
			ToolUseID: "audit_large", Name: "write_file",
			Input: map[string]any{"content": strings.Repeat("X", kiroMaxPayloadBytes+1024)},
		}},
	}
	p := &KiroPayload{ConversationState: KiroConversationState{
		CurrentMessage: KiroCurrentMessage{UserInputMessage: KiroUserInputMessage{
			Content: "continue", ModelID: "claude-sonnet-4.5", Origin: "AI_EDITOR",
		}},
		History: []KiroHistoryMessage{
			{UserInputMessage: &KiroUserInputMessage{Content: "start"}},
			{AssistantResponseMessage: assistant},
			{UserInputMessage: &KiroUserInputMessage{
				Content: "result",
				UserInputMessageContext: &KiroUserInputMessageContext{
					ToolResults: []KiroToolResult{{
						ToolUseID: "audit_large", Status: "success",
						Content: []KiroTextContent{{Text: "done"}},
					}},
				},
			}},
		},
	}}
	before, err := json.Marshal(assistant.ToolUses)
	if err != nil {
		t.Fatal(err)
	}
	err = truncateKiroPayloadToLimit(p, false)
	if err == nil || !strings.Contains(err.Error(), "kiro payload exceeds") {
		t.Fatalf("expected explicit size error, got %v", err)
	}
	after, err := json.Marshal(assistant.ToolUses)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("tool input or call metadata mutated")
	}
}

func assertCandidatePairsAndRecentHistory(t *testing.T, payload []byte) {
	t.Helper()
	pending := map[string]bool{}
	consume := func(results []gjson.Result) {
		for _, result := range results {
			id := result.Get("toolUseId").String()
			if !pending[id] {
				t.Fatalf("unknown or duplicate result: %s", id)
			}
			delete(pending, id)
		}
		if len(pending) != 0 {
			t.Fatal("retained calls lack results")
		}
	}
	retained := 0
	for _, entry := range gjson.GetBytes(payload, "conversationState.history").Array() {
		if a := entry.Get("assistantResponseMessage"); a.Exists() {
			retained++
			if len(pending) != 0 {
				t.Fatal("calls lost before next assistant")
			}
			for _, tool := range a.Get("toolUses").Array() {
				id := tool.Get("toolUseId").String()
				if id == "" || pending[id] {
					t.Fatal("empty or duplicate call ID")
				}
				pending[id] = true
			}
		} else if u := entry.Get("userInputMessage"); u.Exists() {
			if u.Get("content").String() != kiroHistoryTruncationPlaceholder {
				retained++
			}
			consume(u.Get("userInputMessageContext.toolResults").Array())
		}
	}
	consume(gjson.GetBytes(payload,
		"conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults").Array())
	if retained < kiroMinRecentHistoryTurns {
		t.Fatalf("recent history lost: retained=%d minimum=%d",
			retained, kiroMinRecentHistoryTurns)
	}
}
