#!/usr/bin/env bash
# jwt_e2e.sh —— JWT 鉴权 E2E 冒烟脚本（7 步）。
#
# 用法：
#   GATEWAY_URL=http://localhost:8081 bash scripts/jwt_e2e.sh
#
# 依赖：curl, jq
# 退出码：任一步不符合预期 → 非 0，stderr 打印诊断信息。
#
# 脚本只做「链路连通」验证，不做全量语义断言（那是单元/集成测试的职责）。
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8081}"
PHONE="e2e-$(date +%s)"
PASSWORD="password123"
ROLE="rider"

need() { command -v "$1" >/dev/null || { echo "missing dep: $1" >&2; exit 127; }; }
need curl
need jq

# expect_status <http_code> <body_file> <step_name>
expect_status() {
  local code=$1 file=$2 step=$3
  local got
  got=$(jq -r '.__status' "$file")
  if [[ "$got" != "$code" ]]; then
    echo "[FAIL] step=$step expected=$code got=$got body=$(cat "$file")" >&2
    exit 1
  fi
  echo "[OK] step=$step status=$code"
}

# call <METHOD> <PATH> <JSON_BODY> [AUTH_TOKEN]
call() {
  local method=$1 path=$2 body=${3:-} token=${4:-}
  local args=(-s -o /tmp/jwt_e2e.body -w '%{http_code}' -X "$method" "${GATEWAY_URL}${path}")
  args+=(-H 'Content-Type: application/json')
  [[ -n "$token" ]] && args+=(-H "Authorization: Bearer $token")
  [[ -n "$body" ]] && args+=(--data "$body")
  local code
  code=$(curl "${args[@]}")
  # 合并：把 http status 插到 body 的 __status 字段供上游统一解析。
  if ! jq -e . </tmp/jwt_e2e.body >/dev/null 2>&1; then
    printf '{"__status":"%s","raw":%s}\n' "$code" "$(jq -Rs . </tmp/jwt_e2e.body)" >/tmp/jwt_e2e.json
  else
    jq --arg s "$code" '. + {__status: $s}' /tmp/jwt_e2e.body >/tmp/jwt_e2e.json
  fi
}

# 步骤 1：注册
call POST /auth/register "{\"phone\":\"$PHONE\",\"password\":\"$PASSWORD\",\"role\":\"$ROLE\"}"
expect_status 201 /tmp/jwt_e2e.json register

# 步骤 2：登录，拿 access + refresh
call POST /auth/login "{\"phone\":\"$PHONE\",\"password\":\"$PASSWORD\"}"
expect_status 200 /tmp/jwt_e2e.json login
ACCESS=$(jq -r '.data.access_token' /tmp/jwt_e2e.json)
REFRESH=$(jq -r '.data.refresh_token' /tmp/jwt_e2e.json)
EXPIRES_IN=$(jq -r '.data.expires_in' /tmp/jwt_e2e.json)
[[ -n "$ACCESS" && -n "$REFRESH" ]] || { echo "[FAIL] empty tokens" >&2; exit 1; }
[[ "$EXPIRES_IN" =~ ^[0-9]+$ && "$EXPIRES_IN" -gt 0 ]] || { echo "[FAIL] bad expires_in: $EXPIRES_IN" >&2; exit 1; }
echo "[OK] tokens received expires_in=$EXPIRES_IN"

# 步骤 3：/auth/me 不带 token → 401
call GET /auth/me
expect_status 401 /tmp/jwt_e2e.json me_without_token

# 步骤 4：/auth/me 带合法 token → 200
call GET /auth/me "" "$ACCESS"
expect_status 200 /tmp/jwt_e2e.json me_with_token

# 步骤 5：/auth/refresh 旋转
call POST /auth/refresh "{\"refresh_token\":\"$REFRESH\"}"
expect_status 200 /tmp/jwt_e2e.json refresh
NEW_ACCESS=$(jq -r '.data.access_token' /tmp/jwt_e2e.json)
NEW_REFRESH=$(jq -r '.data.refresh_token' /tmp/jwt_e2e.json)
[[ "$NEW_REFRESH" != "$REFRESH" ]] || { echo "[FAIL] refresh not rotated" >&2; exit 1; }
echo "[OK] refresh rotated"

# 步骤 6：/auth/logout 撤销新 access
call POST /auth/logout "{}" "$NEW_ACCESS"
expect_status 200 /tmp/jwt_e2e.json logout

# 步骤 7：登出后 /auth/me → 401
call GET /auth/me "" "$NEW_ACCESS"
expect_status 401 /tmp/jwt_e2e.json me_after_logout

echo "[DONE] JWT E2E passed"
