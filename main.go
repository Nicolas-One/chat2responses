package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)


// readBodyWithLimit 读取请求体，限制最大大小防止恶意请求。
// 超出限制时返回 http.MaxBytesReader 的默认错误响应。
func readBodyWithLimit(r *http.Request) ([]byte, error) {
	maxBodySize := int64(10 << 20) // 10 MB
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodySize)
	return io.ReadAll(r.Body)
}


// setAuthHeader 设置代理请求的 Authorization header。
// 优先使用配置的 API key，否则透传原始请求中的 Authorization header。
func setAuthHeader(proxyReq *http.Request, originalReq *http.Request) {
	authHeader := originalReq.Header.Get("Authorization")
	configuredKey := getAPIKey()
	if configuredKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+configuredKey)
	} else if authHeader != "" {
		proxyReq.Header.Set("Authorization", authHeader)
	}
}

// chatCompletionsHandler 处理 /v1/chat/completions 请求，
// 支持模型转换、tool_call 间隙修复、流式/非流式代理转发。
func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "不允许的请求方法", http.StatusMethodNotAllowed)
		return
	}

	body, err := readBodyWithLimit(r)
	if err != nil {
		http.Error(w, "读取请求体失败或请求体过大", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	if debugMode { log.Printf("[客户端请求] POST /v1/chat/completions\n%s", string(body)) }

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {

		http.Error(w, "无效的 JSON 格式", http.StatusBadRequest)
		return
	}

	req.Model = resolveModel(req.Model)

	// 无论是否做模型转换，转发前始终修复 tool_call 间隙。
	// DeepSeek 拒绝任何 tool_call 缺少 tool 响应消息的请求。
	req.Messages = fixToolCallGaps(req.Messages)

	shouldConvert := shouldConvertModel(req.Model)
	keepReasoning := wantsReasoning(&req)
	req.Messages = ensureReasoningContent(req.Messages, keepReasoning)
	upstreamBody := body
	if shouldConvert {
		convertedReq := convertRequest(&req)
		convertedBody, err := json.Marshal(convertedReq)
		if err != nil {
			http.Error(w, "序列化请求失败", http.StatusInternalServerError)
			return
		}
		upstreamBody = convertedBody
	} else {
		// 强制禁用思考模式时，注入 thinking:disabled；仅模型列表非空时生效
		if getModelList() != "" && getForceDisableThinking() {
			var rawBody map[string]interface{}
			json.Unmarshal(body, &rawBody)
			rawBody["thinking"] = map[string]string{"type": "disabled"}
			delete(rawBody, "reasoning_effort")
			fixedBody, _ := json.Marshal(rawBody)
			upstreamBody = fixedBody
		} else {
			// 不转换模型时，重新序列化以包含占位 tool 响应
			fixedBody, err := json.Marshal(&req)
			if err != nil {
				http.Error(w, "序列化请求失败", http.StatusInternalServerError)
				return
			}
			upstreamBody = fixedBody
		}
	}

	targetURL := getUpstreamURL() + "/chat/completions"
	proxyReq, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		http.Error(w, "创建代理请求失败", http.StatusInternalServerError)
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	setAuthHeader(proxyReq, r)

	client := proxyClient
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "连接上游 API 失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Printf("[chatCompletions] 读取上游错误响应体失败: %v", readErr)
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		if len(errBody) > 0 {
			_, _ = w.Write(errBody)
		}
		return

	}

	if req.Stream {

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		reader := bufio.NewReader(resp.Body)
		doneSeen := false
		clientGone := r.Context().Done()
		for {
			select {
			case <-clientGone:
				return
			default:
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				log.Printf("读取流错误: %v", err)
				return
			}
			// 收到 [DONE] 后丢弃后缀数据
			if doneSeen {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" {
				doneSeen = true
				w.Write([]byte("data: [DONE]\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				continue
			}
			out := line
			if shouldConvert {
				out = convertStreamChunk(line, keepReasoning)
				if out == "" {
					continue
				}
			}
			w.Write([]byte(out))
			w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return
	}
	// 非流式响应
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取响应失败", http.StatusInternalServerError)
		return
	}

	outBody := respBody
	if shouldConvert {
		convertedResp, err := convertResponse(respBody, keepReasoning)
		if err != nil {
			convertedResp = respBody
		}
		outBody = convertedResp
	}

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(outBody)
}

// listModelsHandler 处理 /v1/models 请求，透传上游模型列表。
func listModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "不允许的请求方法", http.StatusMethodNotAllowed)
		return
	}
	targetURL := getUpstreamURL() + "/models"
	proxyReq, err := http.NewRequest(http.MethodGet, targetURL, nil)
	if err != nil {
		http.Error(w, "创建代理请求失败", http.StatusInternalServerError)
		return
	}
	setAuthHeader(proxyReq, r)
	client := proxyClientShort
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "连接上游 API 失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// responsesHandler 将 OpenAI Responses API 请求转为 Chat Completions 并转回响应格式
func responsesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "不允许的请求方法", http.StatusMethodNotAllowed)
		return
	}

	body, err := readBodyWithLimit(r)
	if err != nil {
		http.Error(w, "读取请求体失败或请求体过大", http.StatusBadRequest)
		return
	}
	r.Body.Close()


	// 记录请求体
	log.Printf("[客户端请求] POST /v1/responses\n%s", string(body))

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "无效的 JSON 格式", http.StatusBadRequest)
		return
	}

	// 记录解析后的工具信息
	log.Printf("  工具数量: %d", len(respReq.Tools))
	for i, tool := range respReq.Tools {
		var fnName string
		if tool.Function != nil {
			fnName = tool.Function.Name
		}
		log.Printf("  工具[%d]: type=%s name=%s namespace=%s fn_name=%s", i, tool.Type, tool.Name, tool.NameSpace, fnName)
		if tool.Type == "namespace" {
			desc := tool.Description
			if len(desc) > 200 {
				desc = desc[:200]
			}
			log.Printf("    描述=%s", desc)
			if tool.Parameters != nil {
				paramsStr, _ := json.Marshal(tool.Parameters)
				log.Printf("    参数=%s", string(paramsStr))
			}
		}
	}

	respReq.Model = resolveModel(respReq.Model)

	messages := respReq.Messages
	if len(messages) == 0 {
		messages = responsesInputToMessages(respReq.Input, respReq.Instructions)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Temperature != 0 {
		chatReq.Temperature = respReq.Temperature
	}
	if respReq.MaxTokens != 0 {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != 0 {
		chatReq.TopP = respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools = convertResponsesTools(respReq.Tools)
		if debugMode {
			log.Printf("  转换后的工具数量 (chat.completions): %d", len(chatReq.Tools))
			for i, t := range chatReq.Tools {
				log.Printf("    转换后工具[%d]: type=%s name=%s", i, t.Type, t.Function.Name)
			}
		}
	}
	if respReq.ToolChoice != nil {
		// 	// 转换 Responses tool_choice（含 custom_tool_call namespace 路由）

		chatReq.ToolChoice = convertResponsesToolChoice(respReq.ToolChoice)
	}
	// 将 Responses API reasoning.effort 映射到 Chat Completions
	// Codex 发送 reasoning_effort 作为顶层字段，也支持 reasoning.effort 嵌套
	if !getForceDisableThinking() {
		effort := respReq.ReasoningEffort
		if effort == "" {
			effort = respReq.Reasoning.Effort
		}
		if effort != "" && effort != "none" {
			chatReq.ReasoningEffort = effort
		}
	}

	// deepseek-v4-flash 的 Responses 标准接口默认返回 reasoning，effort=none 也会保留。
	wantReasoning := !getForceDisableThinking()

	// 	// 修复 tool_call 间隙：确保每个 assistant tool_call 后紧跟 tool 响应消息

	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	keepReasoning := wantsReasoning(&chatReq)
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	var upstreamBody []byte
	if shouldConvertModel(chatReq.Model) {
		convertedReq := convertRequest(&chatReq)
		upstreamBody, _ = json.Marshal(convertedReq)
	} else {
		upstreamBody, _ = json.Marshal(&chatReq)
	}

	if debugMode {
		log.Printf("[上游请求体] model=%s", chatReq.Model)
		upstreamObj := map[string]interface{}{}
		json.Unmarshal(upstreamBody, &upstreamObj)
		if tools, ok := upstreamObj["tools"]; ok {
			toolsArr, ok := tools.([]interface{})
			if !ok {
				log.Printf("  上游工具: 意外的类型 %T", tools)
			} else {
				log.Printf("  上游工具数量=%d", len(toolsArr))
				for i, t := range toolsArr {
					if i >= 10 {
						log.Printf("  ...（截断了 %d 个）", len(toolsArr)-10)
						break
					}
					tool := t.(map[string]interface{})
					if fn, ok := tool["function"].(map[string]interface{}); ok {
						log.Printf("  上游工具[%d]: name=%s", i, fn["name"])
					}
				}
			}
		}
		if tc, ok := upstreamObj["tool_choice"]; ok {
			log.Printf("  上游 tool_choice=%v", tc)
		}
	}

	targetURL := getUpstreamURL() + "/chat/completions"
	proxyReq, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	proxyReq.Header.Set("Content-Type", "application/json")
	setAuthHeader(proxyReq, r)

	client := proxyClient
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "连接上游 API 失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if respReq.Stream {
		// ========== 流式响应：直接转发 Body 给流处理器，不缓冲 ==========
		// 上游 streaming /chat/completions 成功时返回 200；
		// 失败时返回 4xx/5xx，需要读取 body 获取错误详情后返回给客户端
		if resp.StatusCode >= 400 {
			respBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				http.Error(w, "读取上游响应失败", http.StatusBadGateway)
				return
			}
			for k, v := range resp.Header {
				for _, val := range v {
					w.Header().Add(k, val)
				}
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(respBody)
			return
		}
		// 直接传原始 Body（流式读取，不缓冲）
		responsesStreamHandler(w, r, resp, respReq.Model, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice)
		return
	}

	// ========== 非流式响应 ==========
	respBody, _ := io.ReadAll(resp.Body)

	if debugMode { log.Printf("[上游 Chat 响应]\n%s", string(respBody)) }
	if resp.StatusCode >= 400 {
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// 转换 Chat Completions 响应为 Responses 格式
	responsesBody := convertChatToResponses(respBody, respReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if debugMode { log.Printf("[客户端响应]\n%s", string(responsesBody)) }
	w.Write(responsesBody)
}

// responsesStreamHandler 将 Chat Completions SSE 流转换为 Responses API SSE 事件流
func responsesStreamHandler(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, upstreamModel string, wantReasoning bool, respTools []ResponsesTool, toolChoice interface{}) {
	debugLog("responsesStreamHandler called", "model", model, "wantReasoning", fmt.Sprintf("%v", wantReasoning))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)

	responseID := "resp_" + time.Now().Format("20060102150405") + "_" + randomString(8)
	reasoningID := "rs_" + responseID
	msgID := "msg_" + responseID + "_0"
	createdAt := time.Now().Unix()
	seq := 0

	reasoningStarted := false
	reasoningDone := false
	messageStarted := false
	messageDone := false
	fullReasoning := ""
	fullText := ""
	totalUsage := map[string]interface{}{}
	createdSent := false
	toolCalls := map[int]map[string]interface{}{}
	toolOrder := []int{}

	messageOutputIndex := func() int {
		if reasoningStarted {
			return 1
		}
		return 0
	}

	reasoningItem := func(status string) map[string]interface{} {
		item := map[string]interface{}{
			"id":      reasoningID,
			"type":    "reasoning",
			"summary": []interface{}{},
		}
		if status != "" {
			item["status"] = status
		}
		if status == "completed" {
			item["encrypted_content"] = ""
		}
		if fullReasoning != "" {
			item["summary"] = []interface{}{map[string]interface{}{"type": "summary_text", "text": fullReasoning}}
		}
		return item
	}

	messageItem := func(status string) map[string]interface{} {
		content := []interface{}{map[string]interface{}{
			"type":        "output_text",
			"annotations": []interface{}{},
			"logprobs":    []interface{}{},
			"text":        fullText,
		}}
		return map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"status":  status,
			"content": content,
			"role":    "assistant",
		}
	}

	emitReasoningDone := func() {
		if !reasoningStarted || reasoningDone {
			return
		}
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]interface{}{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    0,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]interface{}{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    0,
			"summary_index":   0,
			"part":            map[string]interface{}{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]interface{}{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    0,
			"item":            reasoningItem("completed"),
		})
		reasoningDone = true
	}

	emitMessageDone := func() {
		if !messageStarted || messageDone {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_text.done", map[string]interface{}{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText,
			"logprobs":        []interface{}{},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.done", map[string]interface{}{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]interface{}{"type": "output_text", "annotations": []interface{}{}, "logprobs": []interface{}{}, "text": fullText},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]interface{}{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            messageItem("completed"),
		})
		messageDone = true
	}

	emitToolCallDone := func(idx int, call map[string]interface{}) {
		if done, _ := call["done"].(bool); done {
			return
		}
		call["done"] = true
		itemID, _ := call["item_id"].(string)
		callID, _ := call["call_id"].(string)
		name, _ := call["name"].(string)
		ns, _ := call["namespace"].(string)
		args, _ := call["arguments"].(string)
		seq++
		evt := map[string]interface{}{
			"type":            "response.function_call_arguments.done",
			"sequence_number": seq,
			"item_id":         itemID,
			"output_index":    idx,
			"name":            name,
			"arguments":       args,
		}
		if ns != "" {
			evt["namespace"] = ns
		}
		emitSSEEvent(w, flusher, "response.function_call_arguments.done", evt)
		seq++
		item := map[string]interface{}{
			"id":        itemID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": args,
			"call_id":   callID,
			"name":      name,
		}
		if ns != "" {
			item["namespace"] = ns
		}
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]interface{}{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            item,
		})
	}

	clientGone := r.Context().Done()
	for {
		select {
		case <-clientGone:
			debugLog("客户端断连")
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				debugLog("流 EOF")
				break
			}
			debugLog("流错误", "err", err.Error())
			log.Printf("读取流错误: %v", err)
			return
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			debugLog("流 DONE 标记")
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			debugLog("JSON 解析错误", "err", err.Error(), "line", line[:min(len(line), 80)])
			continue
		}
		if !createdSent {
			if id, ok := chunk["id"].(string); ok && id != "" {
				debugLog("收到首个数据块", "id", id[:min(len(id), 20)])
				responseID = id
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
			}
			seq++
			emitSSEEvent(w, flusher, "response.created", map[string]interface{}{
				"type":            "response.created",
				"sequence_number": seq,
				"response":        map[string]interface{}{"id": responseID, "object": "response", "created_at": createdAt, "model": model, "status": "in_progress", "background": false, "error": nil, "output": []interface{}{}},
			})
			seq++
			emitSSEEvent(w, flusher, "response.in_progress", map[string]interface{}{
				"type":            "response.in_progress",
				"sequence_number": seq,
				"response":        map[string]interface{}{"id": responseID, "object": "response", "created_at": createdAt, "model": model, "status": "in_progress"},
			})
			// 			// 标记首个 SSE 事件已发送

			createdSent = true
		}
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				totalUsage = usage
			}
			continue
		}

		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		finishReason, _ := choice["finish_reason"].(string)

		if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
			rcStr, _ := rc.(string)
			if rcStr != "" {
				if !reasoningStarted {
					seq++
					emitSSEEvent(w, flusher, "response.output_item.added", map[string]interface{}{
						"type":            "response.output_item.added",
						"sequence_number": seq,
						"output_index":    0,
						"item":            reasoningItem("in_progress"),
					})
					seq++
					emitSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]interface{}{
						"type":            "response.reasoning_summary_part.added",
						"sequence_number": seq,
						"item_id":         reasoningID,
						"output_index":    0,
						"summary_index":   0,
						"part":            map[string]interface{}{"type": "summary_text", "text": ""},
					})
					reasoningStarted = true
				}
				fullReasoning += rcStr
				seq++
				emitSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]interface{}{
					"type":            "response.reasoning_summary_text.delta",
					"sequence_number": seq,
					"item_id":         reasoningID,
					"output_index":    0,
					"summary_index":   0,
					"delta":           rcStr,
				})
			}
		}

		contentStr := ""
		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ = c.(string)
		}
		if contentStr != "" {
			debugLog("内容增量", "text", contentStr[:min(len(contentStr), 30)])
			emitReasoningDone()
			if !messageStarted {
				idx := messageOutputIndex()
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]interface{}{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    idx,
					"item":            map[string]interface{}{"id": msgID, "type": "message", "status": "in_progress", "content": []interface{}{}, "role": "assistant"},
				})
				seq++
				emitSSEEvent(w, flusher, "response.content_part.added", map[string]interface{}{
					"type":            "response.content_part.added",
					"sequence_number": seq,
					"item_id":         msgID,
					"output_index":    idx,
					"content_index":   0,
					"part":            map[string]interface{}{"type": "output_text", "annotations": []interface{}{}, "logprobs": []interface{}{}, "text": ""},
				})
				messageStarted = true
			}
			fullText += contentStr
			seq++
			emitSSEEvent(w, flusher, "response.output_text.delta", map[string]interface{}{
				"type":            "response.output_text.delta",
				"sequence_number": seq,
				"item_id":         msgID,
				"output_index":    messageOutputIndex(),
				"content_index":   0,
				"delta":           contentStr,
				"logprobs":        []interface{}{},
			})
		}

		rawToolCalls, _ := delta["tool_calls"].([]interface{})
		for _, rawToolCall := range rawToolCalls {
			tc, ok := rawToolCall.(map[string]interface{})
			if !ok {
				continue
			}
			idxFloat, _ := tc["index"].(float64)
			upstreamIndex := int(idxFloat)
			call, exists := toolCalls[upstreamIndex]
			if !exists {
				outputIndex := messageOutputIndex()
				if messageStarted {
					outputIndex++
				}
				outputIndex += len(toolOrder)
				callID, _ := tc["id"].(string)
				if callID == "" {
					callID = "call_" + randomString(12)
				}
			fn, _ := tc["function"].(map[string]interface{})
			rawName, _ := fn["name"].(string)
			if debugMode && rawName != "" {
				log.Printf("[流] 上游 tool_call[%d]: function.name=%q namespace=%q", upstreamIndex, rawName, fn["namespace"])
				// 调试：检查 sequentialthinking 的解码结果
				if inner, ns, ok := decodeNamespacedToolCall(rawName); ok {
					log.Printf("[流]   decoded -> inner=%q ns=%q", inner, ns)
				} else {
					orig := toolNameOriginal(rawName)
					log.Printf("[流]   decode FAILED, toolNameOriginal=%q", orig)
				}
			}
			// 优先将 Codex MCP 扁平化名称解码回 (namespace, inner) 形式。
			name := ""
			ns := ""
			if rawName, ok := fn["name"].(string); ok {
				if inner, decodedNs, ok := decodeNamespacedToolCall(rawName); ok {
					name = inner
					ns = decodedNs
				} else {
					name = toolNameOriginal(rawName)
				}
			}
			// 若上游显式发送了 namespace，保留它（但不覆盖已解码的值）。
			if ns == "" {
				if rawNS, ok := fn["namespace"].(string); ok {
					ns = rawNS
				}
			}
			call = map[string]interface{}{
					"output_index": outputIndex,
					"item_id":      "fc_" + callID,
					"call_id":      callID,
					"name":         name,
					"namespace":    ns,
					"arguments":    "",
					"done":         false,
				}
				// 				// 将新工具调用存入映射表，后续增量更新和完成事件均引用此条目

				toolCalls[upstreamIndex] = call
				toolOrder = append(toolOrder, upstreamIndex)
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]interface{}{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    outputIndex,
					"item": map[string]interface{}{
						"id":        call["item_id"],
						"type":      "function_call",
						"status":    "in_progress",
						"arguments": "",
						"call_id":   callID,
						"name":      name,
					"namespace": ns,
					},
				})
	}
	// 以下代码在外层 for 循环中，对新调用和已有调用都执行增量更新
	fn, _ := tc["function"].(map[string]interface{})
	if rawName, ok := fn["name"].(string); ok {
		if debugMode {
			log.Printf("[流] 上游 tool_call[%d] delta: function.name=%q", upstreamIndex, rawName)
		}
		if inner, decodedNs, ok := decodeNamespacedToolCall(rawName); ok {
			call["name"] = inner
			call["namespace"] = decodedNs
		} else {
			if name := toolNameOriginal(rawName); name != "" {
				call["name"] = name
			}
		}
	}
	// 捕获上游发送的 namespace（Codex 可能依赖 namespace 路由）
	if rawNS, ok := fn["namespace"].(string); ok && rawNS != "" {
		call["namespace"] = rawNS
	}
			if argDelta, _ := fn["arguments"].(string); argDelta != "" {
				call["arguments"] = call["arguments"].(string) + argDelta
				seq++
				emitSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]interface{}{
					"type":            "response.function_call_arguments.delta",
					"sequence_number": seq,
					"item_id":         call["item_id"],
					"output_index":    call["output_index"],
					"delta":           argDelta,
				})
			}
		}

		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			totalUsage = usage
		}
		if finishReason == "stop" || finishReason == "length" || finishReason == "content_filter" || finishReason == "tool_calls" {
			debugLog("收到 finish_reason", "reason", finishReason)
			emitReasoningDone()
			if !messageStarted && len(toolCalls) == 0 {
				idx := messageOutputIndex()
				seq++
				emitSSEEvent(w, flusher, "response.output_item.added", map[string]interface{}{
					"type":            "response.output_item.added",
					"sequence_number": seq,
					"output_index":    idx,
					"item":            map[string]interface{}{"id": msgID, "type": "message", "status": "in_progress", "content": []interface{}{}, "role": "assistant"},
				})
				seq++
				emitSSEEvent(w, flusher, "response.content_part.added", map[string]interface{}{
					"type":            "response.content_part.added",
					"sequence_number": seq,
					"item_id":         msgID,
					"output_index":    idx,
					"content_index":   0,
					"part":            map[string]interface{}{"type": "output_text", "annotations": []interface{}{}, "logprobs": []interface{}{}, "text": ""},
				})
				messageStarted = true
			}
			emitMessageDone()
			for _, idx := range toolOrder {
				emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
			}
		}
	}

	emitReasoningDone()
	emitMessageDone()
	for _, idx := range toolOrder {
		emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
	}

	output := []interface{}{}
	if reasoningStarted {
		output = append(output, reasoningItem("completed"))
	}
	if messageStarted {
		output = append(output, messageItem("completed"))
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		fcItem := map[string]interface{}{
			"id":        call["item_id"],
			"type":      "function_call",
			"status":    "completed",
			"arguments": call["arguments"],
			"call_id":   call["call_id"],
			"name":      call["name"],
		}
		if ns, _ := call["namespace"].(string); ns != "" {
			fcItem["namespace"] = ns
		}
		output = append(output, fcItem)
	}

	completedResponse := map[string]interface{}{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             "completed",
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"output":             output,
	}
	if len(respTools) > 0 {
		completedResponse["tools"] = respTools
	}
	if toolChoice != nil {
		completedResponse["tool_choice"] = toolChoice
	}

	if len(totalUsage) > 0 {
		usage := map[string]interface{}{}
		if v, ok := totalUsage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]interface{}{"cached_tokens": 0}
		}
		if v, ok := totalUsage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := totalUsage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := totalUsage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := totalUsage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		completedResponse["usage"] = usage
	}

	debugLog("completed event", "seq", fmt.Sprintf("%d", seq), "output_count", fmt.Sprintf("%d", len(output)))
	seq++
	emitSSEEvent(w, flusher, "response.completed", map[string]interface{}{
		"type":            "response.completed",
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
}

// emitSSEEvent 发送 SSE 事件
func emitSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("序列化 SSE 事件错误: %v", err)
		return
	}
	w.Write([]byte("event: " + event + "\n"))
	w.Write([]byte("data: " + string(jsonData) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// proxyOtherHandler 处理 /v1/ 下其他路径的请求（如 /v1/embeddings），
// 直接透传请求体和请求头。
func proxyOtherHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/responses" {
		responsesHandler(w, r)
		return
	}
	targetURL := getUpstreamURL() + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.Body != nil {
		bodyBytes, err := readBodyWithLimit(r)
		if err != nil {
			http.Error(w, "读取请求体失败或请求体过大", http.StatusBadRequest)
			return
		}
		body = bytes.NewReader(bodyBytes)
	}
	proxyReq, err := http.NewRequest(r.Method, targetURL, body)
	if err != nil {
		http.Error(w, "创建代理请求失败", http.StatusInternalServerError)
		return
	}
	for k, v := range r.Header {
		for _, val := range v {
			proxyReq.Header.Add(k, val)
		}
	}
	setAuthHeader(proxyReq, r)
	proxyReq.Header.Del("Host")
	client := proxyClient
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "连接上游 API 失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ======================== Admin 管理页面 ========================

// requireAdminAuth 检查管理面板鉴权。
// 当 admin_token 非空时，要求请求携带 Bearer token 或 ?token= 查询参数。
// 鉴权失败返回 401；admin_token 为空时跳过鉴权（本地开发模式）。
func requireAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	token := getAdminToken()
	if token == "" {
		return true // 无需鉴权
	}
	// 1. Authorization: Bearer <token>
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if strings.TrimPrefix(auth, "Bearer ") == token {
			return true
		}
	}
	// 2. ?token=<token>
	if t := r.URL.Query().Get("token"); t == token {
		return true
	}
	// 3. Cookie: admin_token=<token>
	if c, err := r.Cookie("admin_token"); err == nil && c.Value == token {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
	http.Error(w, `{"error":"未授权访问管理面板"}`, http.StatusUnauthorized)
	return false
}

func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := AppConfig{
			UpstreamURL:          getUpstreamURL(),
			APIKey:               getMaskedAPIKey(),
			ModelList:            getModelList(),
			ModelAlias:           getModelAlias(),
			ReasoningEffortMap:   getReasoningEffortMap(),
			ForceDisableThinking: getForceDisableThinking(),
			AdminToken:           getAdminTokenMasked(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)

	case http.MethodPost:
		var cfg AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"无效的 JSON 格式"}`, http.StatusBadRequest)
			return
		}
		if err := saveConfig(configPath, cfg); err != nil {
			http.Error(w, `{"error":"保存配置失败"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(cfg)
		keyPreview := ""
		if len(cfg.APIKey) > 10 {
			keyPreview = cfg.APIKey[:10] + "..."
		} else if cfg.APIKey != "" {
			keyPreview = cfg.APIKey + "..."
		}
		if debugMode { log.Printf("配置已更新：上游=%s，API Key=%s，模型列表=%s，别名=%d 条", cfg.UpstreamURL, keyPreview, cfg.ModelList, len(cfg.ModelAlias)) }
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "不允许的请求方法", http.StatusMethodNotAllowed)
	}
}

// main 程序入口，解析 CLI 参数、加载配置、注册路由、启动 HTTP 服务。
func main() {
	// CLI 参数
	flag.StringVar(&port, "port", "8000", "服务器端口")
	flag.StringVar(&configPath, "config", "config.json", "配置文件路径")
	flag.BoolVar(&debugMode, "debug", false, "启用调试日志")
	flag.Parse()

	// 从配置文件加载所有配置
	cfg := loadConfig(configPath)
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = "https://api.openai.com/v1"
	}
	if cfg.ReasoningEffortMap == nil {
		cfg.ReasoningEffortMap = map[string]string{"low": "high", "medium": "high", "xhigh": "max"}
	}
	applyConfig(cfg)
	saveConfig(configPath, cfg)
	log.Printf("已从 %s 加载配置", configPath)

	log.Printf("DeepSeek OpenAI Proxy Server")
	log.Printf("============================")
	log.Printf("端口:     %s", port)
	log.Printf("上游:     %s", getUpstreamURL())
	log.Printf("转换模型: %s", getModelList())
	log.Printf("模型别名: %d 条", len(getModelAlias()))
	adminAuth := "无鉴权"
	if getAdminToken() != "" {
		adminAuth = "已启用"
	}
	log.Printf("管理页面: http://localhost:%s/admin (%s)", port, adminAuth)
	log.Printf("============================")

	// API 路由
	http.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	http.HandleFunc("/v1/models", listModelsHandler)
	http.HandleFunc("/v1/", proxyOtherHandler)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Admin 路由（鉴权保护）
	http.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		if !requireAdminAuth(w, r) {
			return
		}
		adminPageHandler(w, r)
	})
	http.HandleFunc("/admin/api/config", adminConfigHandler)

	// 根页面重定向到 admin
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	addr := ":" + port
	log.Printf("服务器已启动在 %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
