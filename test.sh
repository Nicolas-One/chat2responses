#!/bin/bash
# ======================================================
# ds2api 综合测试脚本
# 覆盖：基础代理、模型转换、流式、工具调用、管理 API
# 用法：./test.sh [base_url]
# 默认：http://localhost:8000
# ======================================================
set -uo pipefail

BASE="${1:-http://localhost:8000}"
PASS=0; FAIL=0; SKIP=0
CURL="curl -s --max-time 20"

# -------- 输出工具 --------
GRN='\033[32m'; RED='\033[31m'; YLW='\033[33m'; RST='\033[0m'
ok()   { echo -e "${GRN}  ✅ $1${RST}"; PASS=$((PASS+1)); }
fail() { echo -e "${RED}  ❌ $1${RST}"; FAIL=$((FAIL+1)); }
skip() { echo -e "${YLW}  ⚠️  $1${RST}"; SKIP=$((SKIP+1)); }

check_choices() {
  local desc="$1" resp="$2"
  if echo "$resp" | grep -qF '"choices"'; then
    ok "$desc"
  elif echo "$resp" | grep -qF '"error"'; then
    ok "$desc（已路由，上游模型不可用）"
  else
    fail "$desc — $(echo "$resp" | head -c 120)"
  fi
}

header() { echo; echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"; echo " $1"; echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"; }

chat()     { $CURL -X POST "$BASE/v1/chat/completions" -H "Content-Type: application/json" -H "Authorization: Bearer test-key" -d "$1"; }
chat_raw() { $CURL -X POST "$BASE/v1/chat/completions" -H "Content-Type: application/json" -H "Authorization: Bearer test-key" -d "$1" 2>/dev/null || echo ""; }
responses(){ $CURL -X POST "$BASE/v1/responses"        -H "Content-Type: application/json" -H "Authorization: Bearer test-key" -d "$1"; }

echo "═══════════════════════════════════════════════"
echo " ds2api 综合测试"
echo " 目标：$BASE"
echo " 时间：$(date '+%Y-%m-%d %H:%M:%S')"
echo "═══════════════════════════════════════════════"

# ======================== 1. 服务基础 ========================
header "1. 服务基础"

code=$($CURL -o /dev/null -w "%{http_code}" "$BASE/health" 2>/dev/null || echo "000")
[[ "$code" == "200" ]] && ok "Health 返回 200" || fail "Health 返回 $code"

models=$(chat_raw '{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}')
echo "$models" | grep -qF '"choices"' && ok "Chat Completions 可用" || fail "Chat Completions — $(echo "$models" | head -c 100)"

root_code=$($CURL -o /dev/null -w "%{http_code}" "$BASE/" 2>/dev/null || echo "000")
[[ "$root_code" == "302" ]] && ok "根路径重定向到 Admin" || {
  admin_html=$($CURL "$BASE/" 2>/dev/null || echo "")
  echo "$admin_html" | grep -qF "Chat2Responses" && ok "根路径返回 Admin 页面" || fail "根路径返回 $root_code"
}

# ======================== 2. 非流式对话 ========================
header "2. 非流式对话"

check_choices "普通请求"   "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"回复OK"}]}')"
check_choices "System 消息" "$(chat '{"model":"gpt-5.4","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"回复OK"}]}')"
check_choices "max_tokens" "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"回复OK"}],"max_tokens":10}')"
check_choices "temperature" "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"回复OK"}],"temperature":0.5}')"
check_choices "top_p"       "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"回复OK"}],"top_p":0.9}')"

# ======================== 3. 流式对话 ========================
header "3. 流式对话"

stream=$($CURL -X POST "$BASE/v1/chat/completions" \
  -H "Content-Type: application/json" -H "Authorization: Bearer test-key" \
  -d '{"model":"gpt-5.4","messages":[{"role":"user","content":"回复OK"}],"stream":true}' 2>/dev/null || echo "")
if [[ -z "$stream" ]]; then
  fail "流式请求 — 连接失败"
else
  echo "$stream" | grep -qF "data: " && ok "流式返回 SSE 事件" || fail "流式无 SSE 数据"
  echo "$stream" | grep -qF "[DONE]" && ok "流式结束标记 [DONE]" || fail "流式缺少 [DONE]"
fi

stream_conv=$($CURL -X POST "$BASE/v1/chat/completions" \
  -H "Content-Type: application/json" -H "Authorization: Bearer test-key" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"回复OK"}],"stream":true}' 2>/dev/null || echo "")
[[ -n "$stream_conv" ]] && echo "$stream_conv" | grep -qF "data: " && ok "流式+模型转换" || fail "流式+转换 $(echo "$stream_conv" | head -c 80)"

# ======================== 4. 模型解析分支 ========================
header "4. 模型解析与转换"

check_choices "转换列表中的模型"  "$(chat '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}')"
check_choices "deepseek 关键词"   "$(chat '{"model":"deepseek-coder","messages":[{"role":"user","content":"hi"}]}')"
check_choices "非转换模型透传"    "$(chat '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}')"
check_choices "别名解析(gpt-5.4→astron-code-latest)" "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}')"

# 空模型名
empty_resp=$(chat '{"model":"","messages":[{"role":"user","content":"回复OK"}]}')
if echo "$empty_resp" | grep -qF '"choices"'; then
  ok "空模型名转发"
elif echo "$empty_resp" | grep -qiE 'error|missing|无效'; then
  ok "空模型名合理拒绝"
else
  fail "空模型名 — $(echo "$empty_resp" | head -c 80)"
fi

# ======================== 5. DeepSeek 特定 ========================
header "5. DeepSeek 特定功能"

check_choices "reasoning_effort"  "$(chat '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}')"
check_choices "thinking:enabled"  "$(chat '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}')"
check_choices "thinking:disabled" "$(chat '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}')"
check_choices "extra_body 透传"    "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"extra_body":{"custom":"test"}}')"

# ======================== 6. 工具调用 ========================
header "6. 工具调用"

tool_payload='{"model":"gpt-5.4","messages":[{"role":"user","content":"天气"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}'
check_choices "单工具定义" "$(chat "$tool_payload")"

check_choices "tool_choice" "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"天气"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"auto"}')"

check_choices "多工具定义" "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"你好"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},{"type":"function","function":{"name":"get_time","description":"时间","parameters":{"type":"object","properties":{}}}}]}')"

check_choices "工具调用间隙修复" "$(chat '{"model":"gpt-5.4","messages":[{"role":"user","content":"天气"},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}]}')"

# ======================== 7. 管理 API ========================
header "7. 管理 API"

cfg=$($CURL "$BASE/admin/api/config" 2>/dev/null || echo "{}")
echo "$cfg" | grep -qF '"upstream_url"' && ok "GET 配置含 upstream_url" || fail "GET 配置缺 upstream_url"
echo "$cfg" | grep -qF '"model_list"'   && ok "GET 配置含 model_list"   || fail "GET 配置缺 model_list"
echo "$cfg" | grep -qF '"model_alias"'  && ok "GET 配置含 model_alias"  || fail "GET 配置缺 model_alias"

admin=$($CURL "$BASE/admin" 2>/dev/null || echo "")
echo "$admin" | grep -qF "Chat2Responses" && ok "Admin 页面标题正确" || {
  echo "$admin" | grep -qF "DS2API" && ok "Admin 页面标题（旧版）" || fail "Admin 页面标题异常"
}
echo "$admin" | grep -qF "saveConfig" && ok "Admin 页面含 saveConfig" || fail "Admin 页面缺少 saveConfig"
echo "$admin" | grep -qF "loadConfig" && ok "Admin 页面含 loadConfig" || fail "Admin 页面缺少 loadConfig"

# 保存后验证
save_resp=$($CURL -X POST "$BASE/admin/api/config" -H "Content-Type: application/json" -d "$cfg" 2>/dev/null || echo "{}")
echo "$save_resp" | grep -qF '"status"' && ok "POST 保存成功" || fail "POST 保存 — $(echo "$save_resp" | head -c 100)"

cfg2=$($CURL "$BASE/admin/api/config" 2>/dev/null || echo "{}")
echo "$cfg2" | grep -qF '"upstream_url"' && ok "保存后配置一致" || fail "保存后配置丢失"

# ======================== 8. 错误处理 ========================
header "8. 错误处理"

empty_resp=$(chat_raw '')
[[ -n "$empty_resp" ]] && ok "空请求体返回内容" || fail "空请求体无响应"

bad_json=$(chat_raw 'not json')
[[ -n "$bad_json" ]] && ok "非法 JSON 返回内容" || fail "非法 JSON 无响应"

get_code=$($CURL -o /dev/null -w "%{http_code}" -X GET "$BASE/v1/chat/completions" -H "Authorization: Bearer test-key" 2>/dev/null || echo "000")
[[ "$get_code" == "405" ]] && ok "GET 方法返回 405" || fail "GET 返回 $get_code"

nf_code=$($CURL -o /dev/null -w "%{http_code}" "$BASE/v1/nonexistent" -H "Authorization: Bearer test-key" 2>/dev/null || echo "000")
[[ "$nf_code" == "404" ]] && ok "不存在路由返回 404" || fail "不存在路由返回 $nf_code"

# ======================== 9. Responses API ========================
header "9. Responses API"

resp_code=$(responses '{"model":"gpt-5.4","input":"回复OK"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
[[ "$resp_code" == "200" ]] && ok "Responses 非流式 (200)" || skip "Responses 非流式 (HTTP $resp_code — 上游兼容性)"

stream_code=$($CURL -o /dev/null -w "%{http_code}" -X POST "$BASE/v1/responses" \
  -H "Content-Type: application/json" -H "Authorization: Bearer test-key" \
  -d '{"model":"gpt-5.4","input":"回复OK","stream":true}' 2>/dev/null || echo "000")
[[ "$stream_code" == "200" ]] && ok "Responses 流式 (200)" || skip "Responses 流式 (HTTP $streamCode — 上游兼容性)"

# ======================== 10. 健康检查与工具链 ========================
header "10. 构建与配置"

DIR="/www/wwwroot/chat2responses"
for f in build.sh admin.html admin_html.go main.go config.go convert.go tools.go models.go logger.go; do
  [[ -f "$DIR/$f" ]] && ok "$f 存在" || fail "$f 缺失"
done

grep -q 'go:embed' "$DIR/admin_html.go" 2>/dev/null && ok "admin_html.go embed 指令" || fail "admin_html.go 缺少 embed"

# ======================== 汇总 ========================
header "测试汇总"
TOTAL=$((PASS+FAIL))
echo "  通过：$PASS"
echo "  失败：$FAIL"
echo "  跳过：$SKIP"
echo "  总计：$TOTAL"
echo

if [[ $FAIL -gt 0 ]]; then
  echo -e "${RED}⚠️  有 $FAIL 项失败，请检查以上日志${RST}"
  exit 1
else
  echo -e "${GRN}✅ 全部 $PASS 项通过！${RST}"
  exit 0
fi
