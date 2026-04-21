#!/usr/bin/env bash
# 演唱会压测方案 § 3.Step 2.5：k6 → Prometheus remote_write 通路 Smoke 验证脚本。
#
# 职责：
#   1) 检查 ./k6 二进制存在且含 xk6-output-prometheus-remote 扩展；
#   2) 检查 Prometheus 端口可达（假定宿主 9090 已被 Tilt 转发）；
#   3) 跑 30 秒 smoke_remote_write.js，把指标 remote_write 到 Prometheus；
#   4) 用 Prometheus HTTP API 查 k6_http_reqs_total，存在则 PASS，否则 FAIL。
#
# 为什么放 bash 而不做进 k6 脚本：通路检查本质是「外部组件齐了没」，
# bash 配 curl/jq 比 k6 JS 直观，错误定位成本低。

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
K6_BIN="${ROOT}/k6"
PROM_URL="${PROM_URL:-http://localhost:9090}"
SCRIPT="${ROOT}/loadtest/k6/scenarios/smoke_remote_write.js"

log() { printf '[smoke %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { printf '[smoke FAIL] %s\n' "$*" >&2; exit 1; }

# Step 1：二进制检查。
[[ -x "${K6_BIN}" ]] || die "缺少 ${K6_BIN}，请先执行：xk6 build --with github.com/grafana/xk6-output-prometheus-remote"
if ! "${K6_BIN}" version 2>&1 | grep -qi "xk6-output-prometheus-remote"; then
  die "./k6 缺少 xk6-output-prometheus-remote 扩展，请重跑 xk6 build"
fi
log "k6 二进制 OK"

# Step 2：Prometheus 可达性。
if ! curl -fsS "${PROM_URL}/-/ready" >/dev/null 2>&1; then
  die "Prometheus ${PROM_URL} 不可达；请确认 tilt up 已起 prometheus 且 port-forward 到 9090"
fi
log "Prometheus 可达"

# Step 3：跑 Smoke 脚本。
log "开始运行 smoke_remote_write.js（30s）"
"${K6_BIN}" run \
  --out "experimental-prometheus-rw=${PROM_URL}/api/v1/write" \
  "${SCRIPT}"

# Step 4：指标查询。
# 等 10 秒让 remote_write 落盘（scrape_interval 15s，偏保守取 10s）。
log "等待 10s 让 Prometheus 落盘 remote_write 样本"
sleep 10

QUERY='k6_http_reqs_total'
RESULT="$(curl -fsS --data-urlencode "query=${QUERY}" "${PROM_URL}/api/v1/query")"
COUNT="$(printf '%s' "${RESULT}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d.get("data",{}).get("result",[])))')"

if [[ "${COUNT}" -gt 0 ]]; then
  log "PASS：Prometheus 查到 ${COUNT} 条 k6_http_reqs_total 序列"
  exit 0
else
  die "FAIL：Prometheus 查不到 k6_http_reqs_total，通路有断点"
fi
