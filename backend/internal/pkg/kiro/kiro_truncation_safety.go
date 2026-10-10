package kiro

// kiroTailToolResultsPaired verifies that every retained tool_result
// refers to a tool_use in the immediately preceding assistant message.
func kiroTailToolResultsPaired(
	tail []KiroHistoryMessage,
	current KiroUserInputMessage,
) bool {
	pending := make(map[string]bool)

	for _, entry := range tail {
		if assistant := entry.AssistantResponseMessage; assistant != nil {
			pending = make(map[string]bool)
			for _, tool := range assistant.ToolUses {
				pending[tool.ToolUseID] = true
			}
			continue
		}

		if user := entry.UserInputMessage; user != nil {
			if ctx := user.UserInputMessageContext; ctx != nil {
				for _, result := range ctx.ToolResults {
					if !pending[result.ToolUseID] {
						return false
					}
				}
			}
			pending = make(map[string]bool)
		}
	}

	if ctx := current.UserInputMessageContext; ctx != nil {
		for _, result := range ctx.ToolResults {
			if !pending[result.ToolUseID] {
				return false
			}
		}
	}

	return true
}
