package main

import (
	"strings"
)

// qualifyMcpToolName 将 Responses API 的 namespace 工具展平为 ChatCompletions 的单一函数名。
// 示例: ("mcp__api_request__", "send_api_request") -> "mcp__api_request__send_api_request"。
// 如果 ns 不是 mcp__ 前缀格式，则将其视为服务名。
func qualifyMcpToolName(ns string, inner string) string {
	if ns == "" || inner == "" {
		return inner
	}
	if strings.HasPrefix(ns, "mcp__") && strings.HasSuffix(ns, "__") {
		return ns + inner
	}
	return "mcp__" + ns + "__" + inner
}

const maxNsMappings = 1000

// registerNsMapping 注册扁平化函数名到 (namespace, name) 的映射关系，
// 供后续 decodeNamespacedToolCall 查询路由信息。
// 达到 maxNsMappings 上限时清除全部缓存（主动降级到启发式解码）。
func registerNsMapping(flat string, ns string, inner string) {
	if flat == "" || inner == "" || ns == "" {
		return
	}
	toolNameNSMu.Lock()
	defer toolNameNSMu.Unlock()
	if len(toolFlatToNsInner) >= maxNsMappings {
		toolFlatToNsInner = make(map[string]struct {
			Namespace string
			Name      string
		})
	}
	toolFlatToNsInner[flat] = struct {
		Namespace string
		Name      string
	}{Namespace: ns, Name: inner}
}

// decodeNamespacedToolCall 将扁平的 ChatCompletions 函数名还原为 (name, namespace) 形式，
// 前提是它符合 Codex MCP 命名约定。
func decodeNamespacedToolCall(flat string) (string, string, bool) {
	if flat == "" {
		return "", "", false
	}
	toolNameNSMu.RLock()
	m, ok := toolFlatToNsInner[flat]
	toolNameNSMu.RUnlock()
	if ok && m.Name != "" && m.Namespace != "" {
		return m.Name, m.Namespace, true
	}
	// 启发式降级：mcp__{server}__{tool}
	if strings.HasPrefix(flat, "mcp__") {
		rest := strings.TrimPrefix(flat, "mcp__")
		if idx := strings.Index(rest, "__"); idx >= 0 {
			server := rest[:idx]
			tool := rest[idx+2:]
			if server != "" && tool != "" {
				ns := "mcp__" + server + "__"
				return tool, ns, true
			}
		}
	}
	return "", "", false
}

// toolNameOriginal 从函数名中提取裸名（仅保留最后一段），
// 作为 decodeNamespacedToolCall 失败时的保护性降级。
// Codex 的 MCP 工具名格式为 mcp__{server}__{tool}，所以裸名为 __ 之后的部分。
func toolNameOriginal(upstream string) string {
	if upstream == "" || !strings.Contains(upstream, "__") {
		return upstream
	}
	parts := strings.Split(upstream, "__")
	return parts[len(parts)-1]
}

// convertResponsesTools 将 Responses API 的 tools 定义（含 namespace 包装器）
// 转换为 Chat Completions 格式的扁平化 function 工具列表。
func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted := make([]Tool, 0, len(tools))
	seen := map[string]bool{}

	qualifyMcpName := func(ns string, inner string) string {
		return qualifyMcpToolName(ns, inner)
	}

	appendToolFn := func(fn ToolFunction) {
		if fn.Name == "" {
			return
		}
		if seen[fn.Name] {
			return
		}
		seen[fn.Name] = true
		if fn.Parameters == nil {
			fn.Parameters = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		converted = append(converted, Tool{Type: "function", Function: fn})
	}

	for _, tool := range tools {
		if tool.Type == "namespace" {
			ns := tool.Name
			for _, inner := range tool.Tools {
				fn := ToolFunction{
					Name:        inner.Name,
					Description: inner.Description,
					Parameters:  inner.Parameters,
				}
				if inner.Function != nil {
					fn = *inner.Function
				}
				innerName := fn.Name
				if innerName == "" {
					innerName = inner.Name
				}
				flat := qualifyMcpName(ns, innerName)
				fn.Name = flat
				registerNsMapping(flat, ns, innerName)
				appendToolFn(fn)
			}
			continue
		}

		fn := ToolFunction{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		}
		if tool.Function != nil {
			fn = *tool.Function
		}
		name := fn.Name
		if name == "" {
			name = tool.Name
		}
		if tool.NameSpace != "" {
			innerName := name
			flat := qualifyMcpName(tool.NameSpace, innerName)
			name = flat
			registerNsMapping(flat, tool.NameSpace, innerName)
		}
		fn.Name = name
		appendToolFn(fn)
	}
	return converted
}

// convertResponsesToolChoice 将 Responses API 的 tool_choice（含 custom_tool_call）
// 转换为 Chat Completions 格式的 tool_choice。
func convertResponsesToolChoice(choice interface{}) interface{} {
	if choice == nil {
		return nil
	}
	if s, ok := choice.(string); ok {
		return s
	}
	choiceMap, ok := choice.(map[string]interface{})
	if !ok {
		return choice
	}
	if choiceMap["type"] == "function" {
		name, _ := choiceMap["name"].(string)
		if name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
	}
	// custom_tool_call type with namespace
	if choiceMap["type"] == "custom_tool_call" {
		ns, _ := choiceMap["namespace"].(string)
		name, _ := choiceMap["name"].(string)
		if ns != "" && name != "" {
			qn := name
			if strings.HasPrefix(ns, "mcp__") && strings.HasSuffix(ns, "__") {
				qn = ns + name
			} else {
				qn = "mcp__" + ns + "__" + name
			}
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": qn},
			}
		}
		if name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
	}
	return choice
}
