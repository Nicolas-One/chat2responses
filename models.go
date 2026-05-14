package main

// OpenAIRequest Chat Completions 请求结构
type OpenAIRequest struct {
	Model           string                 `json:"model"`
	Messages        []Message              `json:"messages"`
	Stream          bool                   `json:"stream"`
	Temperature     float64                `json:"temperature,omitempty"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	TopP            float64                `json:"top_p,omitempty"`
	Thinking        interface{}            `json:"thinking,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
	ExtraBody       map[string]interface{} `json:"extra_body,omitempty"`
	Tools           []Tool                 `json:"tools,omitempty"`
	ToolChoice      interface{}            `json:"tool_choice,omitempty"`
}

// ResponsesTool Responses API 中的工具定义，支持 namespace 包装器
type ResponsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
	Function    *ToolFunction          `json:"function,omitempty"`
	NameSpace   string                 `json:"namespace,omitempty"`
	Tools       []ResponsesTool        `json:"tools,omitempty"`
}

// Message 聊天消息结构
type Message struct {
	Role             string      `json:"role,omitempty"`
	Content          interface{} `json:"content,omitempty"`
	ToolCalls        []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
	Name             string      `json:"name,omitempty"`
	ReasoningContent *string     `json:"reasoning_content,omitempty"`
}

// ToolCall 上游 Chat Completions 的工具调用
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 工具函数调用
type FunctionCall struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments"`
}

// Tool Chat Completions 工具定义
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 工具函数定义
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// AppConfig 配置文件结构
type AppConfig struct {
	UpstreamURL          string            `json:"upstream_url"`
	APIKey               string            `json:"api_key"`
	ModelList            string            `json:"model_list"`
	ModelAlias           map[string]string `json:"model_alias"`
	ReasoningEffortMap   map[string]string `json:"reasoning_effort_map"`
	ForceDisableThinking bool              `json:"force_disable_thinking"`
	EnableLogging        bool              `json:"enable_logging"`
	AdminToken           string            `json:"admin_token,omitempty"`
}

// ReasonEffort Responses API reasoning 配置
type ReasonEffort struct {
	Effort string `json:"effort,omitempty"`
}

// ResponsesAPIRequest OpenAI Responses API 请求结构
type ResponsesAPIRequest struct {
	Model           string          `json:"model"`
	Input           interface{}     `json:"input"`
	Messages        []Message       `json:"messages,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Temperature     float64         `json:"temperature,omitempty"`
	MaxTokens       int             `json:"max_output_tokens,omitempty"`
	TopP            float64         `json:"top_p,omitempty"`
	Reasoning       ReasonEffort    `json:"reasoning,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Include         []string        `json:"include,omitempty"`
	Store           bool            `json:"store,omitempty"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	ToolChoice      interface{}     `json:"tool_choice,omitempty"`
}
