#!/usr/bin/env bash
# Live end-to-end demo of the EAP journey against a locally running gateway.
#
#   Terminal 1: make dev-infra && ./scripts/demo.sh
#
# The script starts the demo upstream stubs, runs the gateway from source with
# the demo LLM provider wired in, and walks the full journey:
#   login -> create key -> me -> LLM call -> DLP block -> policy deny ->
#   MCP discovery + read tool -> revoke -> everything 401s -> audit dump.
set -euo pipefail
cd "$(dirname "$0")/.."

GW_URL="http://127.0.0.1:18081"
PY() { python3 -c "$1"; }

step() { printf '\n\033[1;36m== %s ==\033[0m\n' "$1"; }

# 0. Infrastructure: compose PG/Redis/MinIO; the compose gateway would fight
#    us for :18080, so stop it — the demo runs the gateway from source.
docker compose -f deploy/docker-compose.yml up -d postgres redis >/dev/null
docker compose -f deploy/docker-compose.yml stop gateway >/dev/null 2>&1 || true

step "0. 启动演示桩（provider :19099 / git :19098）"
python3 scripts/demo_stubs.py >/tmp/eap-demo-stubs.log 2>&1 &
STUB_PID=$!
sleep 1

step "0.1 启动网关（构建后运行，LLM 数据面已接线）"
pkill -f '/tmp/eap-demo-gateway' 2>/dev/null || true
go build -o /tmp/eap-demo-gateway ./apps/gateway
EAP_GATEWAY_ADDR=":18081" \
EAP_DATABASE_URL="postgres://eap:eap@127.0.0.1:15432/eap?sslmode=disable" EAP_REDIS_ADDR="127.0.0.1:16379" \
EAP_DEV_MODE=1 \
EAP_LLM_PROVIDER_URL="http://127.0.0.1:19099" \
EAP_LLM_PROVIDER_KEY="demo-provider-key" \
EAP_LLM_MODELS="daily-fast,coding-high" \
EAP_POLICY_WORKSPACES="secops" \
EAP_MCP_GIT_TOKEN="demo-git-secret-token" \
/tmp/eap-demo-gateway >/tmp/eap-demo-gateway.log 2>&1 &
GW_PID=$!
trap 'kill $GW_PID $STUB_PID 2>/dev/null || true; lsof -ti tcp:18081 2>/dev/null | xargs kill 2>/dev/null || true' EXIT

for i in $(seq 1 30); do
  curl -fsS "$GW_URL/healthz" >/dev/null 2>&1 && break
  sleep 1
done
step "0.2 健康与就绪"
echo "healthz: $(curl -sS $GW_URL/healthz)"
echo "readyz : $(curl -sS $GW_URL/readyz)"

# 1. Login (dev OIDC stand-in)
step "1. 登录（dev-login 同步用户与 Workspace 成员资格）"
SESSION=$(curl -sS -X POST "$GW_URL/enterprise/v1/auth/dev-login" \
  -d '{"email":"demo@example.com","display_name":"Demo 用户","workspaces":["secops"]}' \
  | PY 'import json,sys;print(json.load(sys.stdin)["session_token"])')
echo "session token: ${SESSION:0:40}...(JWT, 仅内存持有)"

# 2. Create an employee key — full value shown exactly once.
step "2. 创建员工 API Key（完整值仅此一次展示）"
KEY_JSON=$(curl -sS -X POST "$GW_URL/enterprise/v1/keys" \
  -H "Authorization: Bearer $SESSION" -d '{"name":"Demo MacBook Harness"}')
echo "$KEY_JSON" | PY 'import json,sys;d=json.load(sys.stdin);print("key_id:",d["key_id"]);print("prefix :",d["prefix"]);print("key    :",d["key"])'
KEY=$(echo "$KEY_JSON" | PY 'import json,sys;print(json.load(sys.stdin)["key"])')

# 3. Who am I
step "3. /enterprise/v1/me（用 agk key 查询身份）"
curl -sS "$GW_URL/enterprise/v1/me" -H "Authorization: Bearer $KEY" | PY 'import json,sys;d=json.load(sys.stdin);print("user:",d["user"]["email"]);print("workspaces:",[w["id"] for w in d["workspaces"]])'

# 4. LLM call
step "4. POST /v1/chat/completions（aud=llm-gateway 换票 → 内核门禁 → Provider）"
curl -sS -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" \
  -d '{"model":"daily-fast","messages":[{"role":"user","content":"总结今天的发布风险"}]}' \
  | PY 'import json,sys;d=json.load(sys.stdin);print("model :",d["model"]);print("content:",d["choices"][0]["message"]["content"]);print("usage :",d["usage"])'

# 5. DLP fails closed
step "5. DLP 拦截：内容含 API Key 形态 → 422 dlp_blocked（不出网）"
curl -sS -o /tmp/dlp.json -w "HTTP %{http_code}\n" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" \
  -d '{"model":"daily-fast","messages":[{"role":"user","content":"my key is sk-abcdefghij0123456789AB"}]}'
PY 'import json;print(json.load(open("/tmp/dlp.json")))'

# 6. Policy denies unlisted model
step "6. 策略拦截：未列模型 → 403 policy_denied"
curl -sS -o /tmp/pol.json -w "HTTP %{http_code}\n" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" \
  -d '{"model":"gpt-x-unlisted","messages":[{"role":"user","content":"hi"}]}'
PY 'import json;print(json.load(open("/tmp/pol.json")))'

# 7. MCP discovery + governed read tool
step "7. MCP：发现（tools/list）与受治理读取（tools/call）"
# 演示桩替换注册表里的占位端点（生产中这是内网真实地址，Agent 不可见）
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U eap -d eap -q -c \
  "UPDATE mcp_tools SET endpoint_url = 'http://127.0.0.1:19098/repos/{repo}/contents/{path}' WHERE name = 'git_read_file';"
curl -sS -X POST "$GW_URL/mcp/gitlab" -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | PY 'import json,sys;d=json.load(sys.stdin);print("tools:",[t["name"] for t in d["result"]["tools"]])'
curl -sS -X POST "$GW_URL/mcp/gitlab" -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"acme/app","path":"README.md"}}}' \
  -o /tmp/mcp.json
PY 'import json;d=json.load(open("/tmp/mcp.json"));print("result:",d["result"]["content"][0]["text"][:120])'
if grep -q "demo-git-secret-token" /tmp/mcp.json; then
  echo "!! credential leaked"; exit 1
fi
echo "凭据检查：响应中不含上游 Git token ✓（token 只注入到上游请求头）"

# 8. Cross-workspace denied
step "8. 跨 Workspace：finance（非成员）→ 403 workspace_forbidden"
curl -sS -o /tmp/ws.json -w "HTTP %{http_code}\n" -X POST "$GW_URL/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: finance" \
  -d '{"model":"daily-fast","messages":[{"role":"user","content":"hi"}]}'
PY 'import json;print(json.load(open("/tmp/ws.json")))'

# 9. Revoke -> everything 401s immediately
step "9. 吊销 Key → 三个面立即 401"
KEY_ID=$(echo "$KEY_JSON" | PY 'import json,sys;print(json.load(sys.stdin)["key_id"])')
CODE=$(curl -sS -o /dev/null -w "%{http_code}" -X DELETE "$GW_URL/enterprise/v1/keys/$KEY_ID" -H "Authorization: Bearer $SESSION")
echo "revoke: HTTP $CODE"
for path in "/enterprise/v1/me" "/v1/chat/completions" "/mcp/gitlab"; do
  METHOD=GET; BODY=""
  if [ "$path" != "/enterprise/v1/me" ]; then
    METHOD=POST; BODY='{"model":"daily-fast","messages":[{"role":"user","content":"hi"}]}'
    if [ "$path" = "/mcp/gitlab" ]; then BODY='{"jsonrpc":"2.0","id":3,"method":"tools/list"}'; fi
  fi
  CODE=$(curl -sS -o /dev/null -w "%{http_code}" -X $METHOD "$GW_URL$path" \
    -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" ${BODY:+-d "$BODY"})
  echo "$path -> HTTP $CODE"
done

# 10. Audit evidence
step "10. 审计证据（audit_events，可靠路径落库）"
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U eap -d eap -c \
  "SELECT type, outcome, count(*) FROM audit_events GROUP BY 1,2 ORDER BY 1,2;"

step "演示完成 ✓"
echo "网关日志: /tmp/eap-demo-gateway.log  桩日志: /tmp/eap-demo-stubs.log"
