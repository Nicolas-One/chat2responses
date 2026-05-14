# Chat2Responses

Chat Completions -> OpenAI Responses API 转换代理，支持 DeepSeek 特定优化。

## 功能

- `/v1/chat/completions` — Chat Completions 代理，支持流式/非流式
- `/v1/responses` — Responses API 转换（将 Responses 请求转为 Chat Completions，再将响应转回 Responses 格式）
- `/v1/models` — 透传上游模型列表
- `/v1/*` — 其他路径直接透传
- `/admin` — Web 管理面板，可修改配置无需重启

### 核心特性

- 模型别名映射（如 `gpt-5.4` → `astron-code-latest`）
- DeepSeek reasoning/thinking 格式转换
- MCP namespace 工具展平与还原
- tool_call 间隙自动修复（DeepSeek 要求每个 tool_call 紧跟 tool 响应）
- 请求体大小限制（10MB）
- SSE 流客户端断连检测
- Admin API Key 脱敏展示

## 开源协议

本项目采用 [Apache License 2.0](LICENSE) 开源协议。

你可以自由使用、修改和分发本软件，但**必须保留原作者版权声明和归属信息**。修改后的文件需标注变更说明。

## 构建

```bash
./build.sh
```

需要 Go 1.16+。

## 运行

```bash
./chat2responses -port 8000 -config config.json
```

首次运行会自动生成 `config.json`，或从模板复制：

```bash
cp config.json.example config.json
```

模板内容：

```json
{
  "upstream_url": "https://your-upstream-api/v1",
  "api_key": "",
  "model_list": "",
  "model_alias": {},
  "reasoning_effort_map": {"low": "high", "medium": "high", "xhigh": "max"},
  "force_disable_thinking": false,
  "enable_logging": false,
  "admin_token": ""
}
```

## 测试

```bash
./test.sh [base_url]
```

默认测试 `http://localhost:8000`。

## 安全提示

- `config.json` 包含 API Key，已在 `.gitignore` 中排除，**不要提交到仓库**
- Admin 面板支持 Token 鉴权：在 `config.json` 中设置 `admin_token` 后，访问 `/admin` 需携带该 Token
  - Bearer header：`Authorization: Bearer <admin_token>`
  - 查询参数：`/admin?token=<admin_token>`
  - Cookie：`admin_token=<admin_token>`（管理页面自动使用）
  - `admin_token` 为空时无鉴权，仅建议本地开发使用
- Admin API Key 在 GET 响应中脱敏展示（仅返回末 4 位）
- 请求体限制为 10MB，防止恶意大请求

## 配置说明

| 字段 | 说明 |
|------|------|
| `upstream_url` | 上游 API 基地址 |
| `api_key` | 优先使用的 API Key，为空则透传客户端 Authorization |
| `model_list` | 需 DeepSeek 格式转换的模型（逗号分隔） |
| `model_alias` | 模型别名映射 |
| `reasoning_effort_map` | reasoning_effort 值映射 |
| `force_disable_thinking` | 强制禁用思考模式 |
| `enable_logging` | 启用日志写入文件 |
| `admin_token` | 管理面板鉴权 Token，为空则无鉴权 |
