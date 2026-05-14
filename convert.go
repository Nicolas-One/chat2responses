package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// resolveModel 根据模型别名表解析模型名称，未匹配则返回原值。
func resolveModel(model string) string {
	m := strings.TrimSpace(model)
	aliases := getModelAliasFast()
	if alias, ok := aliases[m]; ok {
		return alias
	}
	return m
}

// shouldConvertModel 判断是否需要对模型进行 DeepSeek 特定优化转换。
// 匹配条件：模型名在 config.model_list 列表中，或模型名包含 "deepseek" 关键词。
// 若返回 false，则使用通用格式转换（不注入 thinking/reasoning_effort 等 DeepSeek 专用字段）。
func shouldConvertModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if strings.Contains(strings.ToLower(model), "deepseek") {
		return true
	}
	for _, candidate := range strings.Split(getModelList(), ",") {
		if strings.TrimSpace(candidate) == model {
			return true
		}
	}
	return false
}

// isThinkingEnabled 判断 thinking 配置是否为启用状态。
func isThinkingEnabled(value interface{}) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		t, _ := v["type"].(string)
		return t == "enabled"
	case bool:
		return v
	default:
		return false
	}
}

// isThinkingDisabled 判断 thinking 配置是否为禁用状态。
func isThinkingDisabled(value interface{}) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		t, _ := v["type"].(string)
		return t == "disabled"
	case bool:
		return !v
	default:
		return false
	}
}

// wantsReasoning 判断是否需要从上游响应中提取 reasoning_content。
// DeepSeek 路径：根据请求和全局配置判断；通用路径：始终保留（由上游决定是否提供）。
func wantsReasoning(req *OpenAIRequest) bool {
	if getForceDisableThinking() {
		return false
	}
	if isThinkingDisabled(req.Thinking) {
		return false
	}
	if isThinkingEnabled(req.Thinking) {
		return true
	}
	if req.ExtraBody != nil {
		if isThinkingDisabled(req.ExtraBody["thinking"]) {
			return false
		}
		if isThinkingEnabled(req.ExtraBody["thinking"]) {
			return true
		}
	}
	return true
}

// normalizeContent 将 Message.Content 统一为 *string 格式，支持 string、[]interface{} 等多种输入。
func normalizeContent(content interface{}) *string {
	if content == nil {
		return nil
	}
	switch v := content.(type) {
	case string:
		return &v
	case []interface{}:
		var parts []string
		for _, part := range v {
			if p, ok := part.(map[string]interface{}); ok {
				if text, ok := p["text"].(string); ok {
					parts = append(parts, text)
					continue
				}
				if text, ok := p["content"].(string); ok {
					parts = append(parts, text)
					continue
				}
			}
			if text, ok := part.(string); ok {
				parts = append(parts, text)
			}
		}
		joined := strings.Join(parts, "\n")
		return &joined
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		s := string(b)
		return &s
	}
}

// fixToolCallGaps 重排消息，确保每个 assistant tool_call 紧跟其对应的 tool 响应。
// DeepSeek 不允许 tool 响应与对应 tool_call 之间插入其他类型消息。
func fixToolCallGaps(messages []Message) []Message {
	// 快速路径：没有任何 tool 消息时直接返回原始切片
	hasTool := false
	for _, msg := range messages {
		if msg.Role == "tool" || (msg.Role == "assistant" && len(msg.ToolCalls) > 0) {
			hasTool = true
			break
		}
	}
	if !hasTool {
		return messages
	}

	toolResponses := map[string]*Message{}
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID != "" {
			toolResponses[messages[i].ToolCallID] = &messages[i]
		}
	}

	fixed := make([]Message, 0, len(messages)+len(messages)/4)
	emitted := map[string]bool{}
	pending := map[string]Message{}

	for _, msg := range messages {
		// 跳过原始位置中的 tool 消息：它们会在对应 assistant tool_call 之后重新插入，
		// 避免工具消息出现在 assistant 之前时被重复输出。
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if !emitted[msg.ToolCallID] {
				// 尚未遇到对应 assistant，暂存到 pending，由后续 assistant 触发插入
				pending[msg.ToolCallID] = msg
			}
			continue
		}
		fixed = append(fixed, msg)

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if pendingMsg, found := pending[tc.ID]; found {
					fixed = append(fixed, pendingMsg)
					delete(pending, tc.ID)
					emitted[tc.ID] = true
				} else if resp, found := toolResponses[tc.ID]; found {
					fixed = append(fixed, *resp)
					emitted[tc.ID] = true
				} else {
					fixed = append(fixed, Message{
						Role:       "tool",
						ToolCallID: tc.ID,
						Content:    "Tool call result not available",
					})
					emitted[tc.ID] = true
				}
			}
		}
	}
	return fixed
}

// ensureReasoningContent 确保每条 assistant 消息都带有 reasoning_content 字段。
// DeepSeek 要求思维模式下每条 assistant 消息都包含此字段。
func ensureReasoningContent(messages []Message, thinking bool) []Message {
	if !thinking {
		return messages
	}
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].ReasoningContent == nil {
			empty := ""
			messages[i].ReasoningContent = &empty
		}
	}
	return messages
}

// convertMessagesForDeepSeek 将 Message 数组转换为 DeepSeek API 所需的 map 格式。
func convertMessagesForDeepSeek(messages []Message) []map[string]interface{} {
	converted := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		clean := map[string]interface{}{}
		if msg.Role != "" {
			clean["role"] = msg.Role
		}
		content := normalizeContent(msg.Content)
		reasoningContent := msg.ReasoningContent
		if content != nil {
			clean["content"] = *content
		}
		if reasoningContent != nil {
			clean["reasoning_content"] = *reasoningContent
		}
		if len(msg.ToolCalls) > 0 {
			clean["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			clean["tool_call_id"] = msg.ToolCallID
		}
		if msg.Name != "" {
			clean["name"] = msg.Name
		}
		converted = append(converted, clean)
	}
	return converted
}

// convertRequest 将 OpenAIRequest 转换为 DeepSeek 格式的请求体。
func convertRequest(req *OpenAIRequest) map[string]interface{} {
	converted := map[string]interface{}{
		"model":    req.Model,
		"messages": convertMessagesForDeepSeek(req.Messages),
		"stream":   req.Stream,
	}
	if req.Temperature != 0 {
		converted["temperature"] = req.Temperature
	}
	if req.MaxTokens != 0 {
		converted["max_tokens"] = req.MaxTokens
	}
	if req.TopP != 0 {
		converted["top_p"] = req.TopP
	}
	if len(req.Tools) > 0 {
		converted["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		converted["tool_choice"] = req.ToolChoice
	}
	if getForceDisableThinking() || isThinkingDisabled(req.Thinking) {
		converted["thinking"] = map[string]string{"type": "disabled"}
	} else {
		converted["thinking"] = map[string]string{"type": "enabled"}
	}
	if !getForceDisableThinking() && req.ReasoningEffort != "" {
		effortMap := getReasoningEffortMap()
		if mapped, ok := effortMap[req.ReasoningEffort]; ok {
			converted["reasoning_effort"] = mapped
		} else {
			converted["reasoning_effort"] = req.ReasoningEffort
		}
	}
	if req.ExtraBody != nil {
		for k, v := range req.ExtraBody {
			if _, exists := converted[k]; !exists {
				converted[k] = v
			}
		}
	}
	return converted
}

// genericConvertRequest 将 OpenAIRequest 转为通用的 Chat Completions 请求体，不注入特定模型字段。
func genericConvertRequest(req *OpenAIRequest) map[string]interface{} {
	converted := map[string]interface{}{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   req.Stream,
	}
	if req.Temperature != 0 {
		converted["temperature"] = req.Temperature
	}
	if req.MaxTokens != 0 {
		converted["max_tokens"] = req.MaxTokens
	}
	if req.TopP != 0 {
		converted["top_p"] = req.TopP
	}
	if len(req.Tools) > 0 {
		converted["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		converted["tool_choice"] = req.ToolChoice
	}
	if req.ReasoningEffort != "" {
		converted["reasoning_effort"] = req.ReasoningEffort
	}
	if req.ExtraBody != nil {
		for k, v := range req.ExtraBody {
			if _, exists := converted[k]; !exists {
				converted[k] = v
			}
		}
	}
	return converted
}

// cleanNulls 移除 map 中值为 null 或空字符串的键。
func cleanNulls(m map[string]interface{}) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
}

// cleanStreamDelta 清理 SSE delta 中的空值和不需要的字段。
func cleanStreamDelta(delta map[string]interface{}, keepReasoning bool) {
	if v, ok := delta["content"]; ok && v == nil {
		delete(delta, "content")
	}
	if s, ok := delta["content"].(string); ok && s == "" {
		delete(delta, "content")
	}
	if !keepReasoning {
		delete(delta, "reasoning_content")
	} else {
		if v, ok := delta["reasoning_content"]; ok && v == nil {
			delete(delta, "reasoning_content")
		}
		if s, ok := delta["reasoning_content"].(string); ok && s == "" {
			delete(delta, "reasoning_content")
		}
	}
}

// convertStreamChunk 转换 Chat Completions SSE 数据行。
func convertStreamChunk(line string, keepReasoning bool) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		return line
	}
	if !strings.HasPrefix(line, "data: ") {
		return line
	}
	data := line[6:]

	// 快速预检：只有行中包含我们会处理的字段时才解码 JSON
	// 不包含这些字段的 chunk（如纯状态/空 delta）直接透传，跳过昂贵的 Unmarshal→Marshal
	if !strings.Contains(data, "\"reasoning_content") &&
		!strings.Contains(data, "\"logprobs") &&
		!strings.Contains(data, "\"usage") &&
		!strings.Contains(data, "\"cost") &&
		!strings.Contains(data, "\"system_fingerprint") &&
		!strings.Contains(data, "\"finish_reason") {
		return line
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return line
	}

	choices, ok := raw["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}

	for i, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			cleanStreamDelta(delta, keepReasoning)
			choice["delta"] = delta
		}
		if msg, ok := choice["message"].(map[string]interface{}); ok {
			cleanNulls(msg)
			if !keepReasoning {
				delete(msg, "reasoning_content")
			}
			choice["message"] = msg
		}
		if v, ok := choice["logprobs"]; ok && v == nil {
			delete(choice, "logprobs")
		}
		if v, ok := choice["finish_reason"]; ok && v == nil {
			delete(choice, "finish_reason")
		}
		if s, ok := choice["finish_reason"].(string); ok && s == "" {
			delete(choice, "finish_reason")
		}
		choices[i] = choice
	}
	raw["choices"] = choices

	if v, ok := raw["usage"]; ok && v == nil {
		delete(raw, "usage")
	}
	delete(raw, "cost")

	converted, err := json.Marshal(raw)
	if err != nil {
		return line
	}
	return "data: " + string(converted)
}

// convertResponse 转换非流式 Chat Completions 响应体。
func convertResponse(data []byte, keepReasoning bool) ([]byte, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return data, nil
	}

	if choices, ok := raw["choices"].([]interface{}); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]interface{}); ok {
				if msg, ok := choice["message"].(map[string]interface{}); ok {
					cleanNulls(msg)
					if !keepReasoning {
						delete(msg, "reasoning_content")
					}
					choice["message"] = msg
				}
				if v, ok := choice["logprobs"]; ok && v == nil {
					delete(choice, "logprobs")
				}
				choices[i] = choice
			}
		}
		raw["choices"] = choices
	}

	if usage, ok := raw["usage"].(map[string]interface{}); ok {
		cleanU := map[string]interface{}{
			"prompt_tokens":     usage["prompt_tokens"],
			"completion_tokens": usage["completion_tokens"],
			"total_tokens":      usage["total_tokens"],
		}
		raw["usage"] = cleanU
	}

	delete(raw, "cost")
	delete(raw, "system_fingerprint")

	return json.Marshal(raw)
}

// collectFunctionOutputs 从 Responses input 中提取所有 function_call_output，按 call_id 建表。
func collectFunctionOutputs(items []interface{}) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]interface{})
		if !ok || elem["type"] != "function_call_output" {
			continue
		}
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			continue
		}
		switch v := elem["output"].(type) {
		case string:
			outputs[callID] = v
		default:
			b, _ := json.Marshal(v)
			outputs[callID] = string(b)
		}
	}
	return outputs
}

// responsesInputToMessages 将 Responses API input 数组转为 Chat Completions Message 数组。
func responsesInputToMessages(input interface{}, instructions string) []Message {
	var messages []Message
	if instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []interface{}:
		functionOutputs := collectFunctionOutputs(v)
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: elem})
			case map[string]interface{}:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["id"].(string)
					}
					name, _ := elem["name"].(string)
					args, _ := elem["arguments"].(string)
					messages = append(messages, Message{
						Role:    "assistant",
						Content: "",
						ToolCalls: []ToolCall{{
							ID:   callID,
							Type: "function",
							Function: FunctionCall{
								Name:      name,
								Arguments: args,
							},
						}},
					})
					if callID != "" {
						output := functionOutputs[callID]
						if output == "" {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
				case "function_call_output":
					continue
				case "reasoning":
					continue
				case "message", "":
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					if role == "developer" {
						role = "system"
					}
					text := extractTextFromContentParts(elem["content"])
					messages = append(messages, Message{Role: role, Content: text})
				default:
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					text := extractTextFromContentParts(elem["content"])
					if text == "" {
						b, _ := json.Marshal(elem)
						text = string(b)
					}
					messages = append(messages, Message{Role: role, Content: text})
				}
			default:
				b, _ := json.Marshal(elem)
				messages = append(messages, Message{Role: "user", Content: string(b)})
			}
		}
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

// extractTextFromContentParts 从 Responses API content 数组中提取文本。
func extractTextFromContentParts(content interface{}) string {
	parts, ok := content.([]interface{})
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	var texts []string
	for _, p := range parts {
		if part, ok := p.(map[string]interface{}); ok {
			if part["type"] == "input_text" || part["type"] == "output_text" {
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

var (
	debugLogFile *os.File
	debugLogMu   sync.Mutex
)

// debugLog 写入调试日志到 /tmp/chat2responses-debug.log，支持键值对参数。
func debugLog(msg string, args ...string) {
	debugLogMu.Lock()
	defer debugLogMu.Unlock()
	if debugLogFile == nil {
		var err error
		debugLogFile, err = os.OpenFile("/tmp/chat2responses-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
	}
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000"), msg)
	for i := 0; i+1 < len(args); i += 2 {
		line += fmt.Sprintf(" %s=%s", args[i], args[i+1])
	}
	debugLogFile.WriteString(line + "\n")
}

// randomString 生成随机字符串（用于 call_id 等）。
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	t := time.Now().UnixNano()
	for i := range b {
		b[i] = letters[t%int64(len(letters))]
		t = t/int64(len(letters)) + int64(i*37)
		if t == 0 {
			t = time.Now().UnixNano() + int64(i)
		}
	}
	return string(b)
}

// convertChatToResponses 将 Chat Completions 响应转换为 Responses API 格式。
func convertChatToResponses(chatBody []byte, model string, wantReasoning bool, respTools []ResponsesTool, toolChoice interface{}) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]interface{} `json:"usage"`
	}
	json.Unmarshal(chatBody, &chat)

	text := ""
	reasoning := ""
	var toolCalls []ToolCall
	if len(chat.Choices) > 0 {
		text = chat.Choices[0].Message.Content
		if wantReasoning {
			reasoning = chat.Choices[0].Message.ReasoningContent
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
	}

	responses := map[string]interface{}{
		"id":                 chat.ID,
		"object":             "response",
		"status":             "completed",
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(respTools) > 0 {
		responses["tools"] = respTools
	}
	if toolChoice != nil {
		responses["tool_choice"] = toolChoice
	}
	outputID := "msg_" + chat.ID + "_0"

	output := []interface{}{}
	if reasoning != "" {
		output = append(output, map[string]interface{}{
			"id":                "rs_" + chat.ID,
			"type":              "reasoning",
			"encrypted_content": "",
			"summary":           []interface{}{map[string]interface{}{"type": "summary_text", "text": reasoning}},
		})
	}
	if text != "" {
		output = append(output, map[string]interface{}{
			"id":     outputID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []interface{}{map[string]interface{}{
				"type":        "output_text",
				"text":        text,
				"annotations": []interface{}{},
				"logprobs":    []interface{}{},
			}},
		})
	}
	for _, tc := range toolCalls {
		fcItem := map[string]interface{}{
			"id":        "fc_" + tc.ID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": tc.Function.Arguments,
			"call_id":   tc.ID,
		}
		if inner, ns, ok := decodeNamespacedToolCall(tc.Function.Name); ok {
			fcItem["name"] = inner
			fcItem["namespace"] = ns
		} else {
			fcItem["name"] = toolNameOriginal(tc.Function.Name)
		}
		output = append(output, fcItem)
	}
	responses["output"] = output
	if chat.Usage != nil {
		usage := map[string]interface{}{}
		if v, ok := chat.Usage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]interface{}{"cached_tokens": 0}
		}
		if v, ok := chat.Usage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := chat.Usage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := chat.Usage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := chat.Usage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		responses["usage"] = usage
	}

	result, _ := json.Marshal(responses)
	return result
}
