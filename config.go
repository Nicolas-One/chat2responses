package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	upstreamURL          string
	apiKey               string
	port                 string
	modelList            string
	configPath           = "config.json"
	modelAlias           = map[string]string{}
	reasoningEffortMap   = map[string]string{
		"low":    "high",
		"medium": "high",
		"xhigh":  "max",
	}
	forceDisableThinking bool
	debugMode            bool
	adminToken           string
	configMu             sync.RWMutex
	toolNameNSMu         sync.RWMutex
	// toolFlatToNsInner 将扁平的 ChatCompletions 函数名（如 mcp__api_request__send_api_request）
	// 映射回 Responses API 的表示形式（namespace + inner tool name）。
	toolFlatToNsInner = make(map[string]struct {
		Namespace string
		Name      string
	})
	// 共享 HTTP 客户端，复用连接池
	proxyClient = &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
		},
	}
	proxyClientShort = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
		},
	}
)

// loadConfig 从 JSON 配置文件加载 AppConfig，文件不存在时返回空配置。
func loadConfig(path string) AppConfig {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

// saveConfig 将 AppConfig 以 JSON 格式写入配置文件（权限 0600）。
func saveConfig(path string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// applyConfig 将 AppConfig 配置项应用到全局变量（线程安全）。
func applyConfig(cfg AppConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg.UpstreamURL != "" {
		upstreamURL = cfg.UpstreamURL
	}
	if cfg.APIKey != "" {
		apiKey = cfg.APIKey
	}
	// ModelList 允许为空字符串（用户清空保存后不恢复旧值）
	modelList = cfg.ModelList
	if cfg.ModelAlias != nil {
		modelAlias = cfg.ModelAlias
	}
	if cfg.ReasoningEffortMap != nil {
		reasoningEffortMap = cfg.ReasoningEffortMap
	}
	forceDisableThinking = cfg.ForceDisableThinking
	adminToken = cfg.AdminToken
	initLogging(cfg.EnableLogging)
}

// getUpstreamURL 线程安全地获取上游 API 地址。
func getUpstreamURL() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return upstreamURL
}

// getAPIKey 线程安全地获取 API 密钥。
func getAPIKey() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return apiKey
}

// getMaskedAPIKey 返回脱敏后的 API Key（仅返回末 4 位），用于管理页面展示。
func getMaskedAPIKey() string {
	key := getAPIKey()
	if len(key) <= 8 {
		return key
	}
	return "****" + key[len(key)-4:]
}

// getAdminTokenMasked 返回脱敏后的登录密码，用于管理页面展示。
func getAdminTokenMasked() string {
	token := getAdminToken()
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "****"
	}
	return "****" + token[len(token)-4:]
}

// getModelList 线程安全地获取需要转换的模型列表。
func getModelList() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return modelList
}

// getModelAlias 线程安全地获取模型别名映射表（深拷贝，供 admin API 用）。
func getModelAlias() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make(map[string]string, len(modelAlias))
	for k, v := range modelAlias {
		cp[k] = v
	}
	return cp
}

// getModelAliasFast 线程安全地获取模型别名映射表（无拷贝，供 hot path 只读使用）。
func getModelAliasFast() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	return modelAlias
}

// getReasoningEffortMap 线程安全地获取 reasoning_effort 映射表。
func getReasoningEffortMap() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make(map[string]string, len(reasoningEffortMap))
	for k, v := range reasoningEffortMap {
		cp[k] = v
	}
	return cp
}

// getForceDisableThinking 线程安全地获取强制禁用思考模式的标志。
func getForceDisableThinking() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return forceDisableThinking
}

// getAdminToken 线程安全地获取登录密码。
func getAdminToken() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return adminToken
}
