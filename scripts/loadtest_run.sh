#!/usr/bin/env bash
# 演唱会压测方案 § 3.Step 10：一键执行脚本。
#
# 职责：按顺序跑 preflight → seed → warmup → chaos workflow 并发 → concert.js → 归档 summary。
# 三轮模式由 --round 参数选：
#   --round 1  Dry Run    : 1/10 流量，不启 chaos（验证脚本通）
#   --round 2  Baseline   : 全流量，不启 chaos（K6_PROFILE=baseline 严格阈值）
#   --round 3  Full Chaos : 全流量，启 chaos workflow（K6_PROFILE=chaos 宽松阈值）
#
# 运行示例：
#   ./scripts/loadtest_run.sh --round 1
#   ./scripts/loadtest_run.sh --round 2
#   ./scripts/loadtest_run.sh --round 3
#
# 前置：
#   1) tilt up 已启全部服务，包括 Prometheus / Grafana / 目标业务服务；
#   2) Chaos Mesh 已装（loadtest/install_chaos_mesh.sh）；
#   3) xk6 已产出仓库根 ./k6。

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
K6="${ROOT}/k6"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8081}"
PROM_URL="${PROM_URL:-http://localhost:9090}"
NS_CHAOS="${NS_CHAOS:-chaos-mesh}"

ROUND=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --round) ROUND="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done
[[ -z "${ROUND}" ]] && { echo "usage: $0 --round {1|2|3}" >&2; exit 2; }

log() { printf '[loadtest %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { printf '[loadtest FAIL] %s\n' "$*" >&2; exit 1; }

# -- preflight -----------------------------------------------------------------
log "preflight: 检查 ./k6 / Prometheus / Gateway 可达性"
[[ -x "${K6}" ]] || die "./k6 不存在，请先跑 xk6 build"
curl -fsS "${PROM_URL}/-/ready" >/dev/null || die "Prometheus 未就绪（${PROM_URL}）"
curl -fsS -o /dev/null "${GATEWAY_URL}/auth/login" -X POST -H 'Content-Type: application/json' -d '{}' \
  || log "gateway /auth/login 预探测失败（可能返回 401 也算正常），若后续 login 异常请检查"

# -- seed ----------------------------------------------------------------------
USERS_JSON="${ROOT}/loadtest/k6/seed/users.json"
if [[ ! -s "${USERS_JSON}" ]]; then
  log "未发现 users.json，开始执行种子脚本"
  (cd "${ROOT}" && GATEWAY_URL="${GATEWAY_URL}" go run ./loadtest/k6/seed/seed.go)
else
  log "检测到已存在 users.json，跳过 seed（如需重跑请手动删）"
fi

# -- warmup --------------------------------------------------------------------
log "warmup: 30s 低强度打流预热连接池"
"${K6}" run \
  -o "experimental-prometheus-rw=${PROM_URL}/api/v1/write" \
  --env "GATEWAY_URL=${GATEWAY_URL}" \
  "${ROOT}/loadtest/k6/scenarios/warmup.js"

# -- round dispatch ------------------------------------------------------------
SUMMARY_DIR="${ROOT}/loadtest/report"
mkdir -p "${SUMMARY_DIR}"
TS="$(date +%Y%m%d-%H%M%S)"
SUMMARY_FILE="${SUMMARY_DIR}/round-${ROUND}-summary-${TS}.json"

case "${ROUND}" in
  1)
    log "Round 1 Dry Run：用 smoke.js 内置 3/2/5 VUs scenario，不启 chaos"
    # 注意：不要带 --vus / --duration，它会覆盖 smoke.js 里的 scenarios 导致找不到 default。
    "${K6}" run \
      -o "experimental-prometheus-rw=${PROM_URL}/api/v1/write" \
      --env "GATEWAY_URL=${GATEWAY_URL}" \
      --env "K6_PROFILE=baseline" \
      --summary-export="${SUMMARY_FILE}" \
      "${ROOT}/loadtest/k6/scenarios/smoke.js"
    ;;
  2)
    log "Round 2 Baseline：全流量，不启 chaos"
    "${K6}" run \
      -o "experimental-prometheus-rw=${PROM_URL}/api/v1/write" \
      --env "GATEWAY_URL=${GATEWAY_URL}" \
      --env "K6_PROFILE=baseline" \
      --summary-export="${SUMMARY_FILE}" \
      "${ROOT}/loadtest/k6/scenarios/concert.js"
    ;;
  3)
    log "Round 3 Full Chaos：全流量 + chaos workflow"
    # 并发启动：先后台 apply chaos workflow（自带 8min Suspend，不影响爬升期），
    # 再前台跑 concert.js（17min）；结束后必定 cleanup 保证不残留 CR。
    kubectl apply -f "${ROOT}/loadtest/chaos/schedule.yaml" -n "${NS_CHAOS}"
    cleanup() {
      log "cleanup chaos workflow"
      kubectl delete -f "${ROOT}/loadtest/chaos/schedule.yaml" -n "${NS_CHAOS}" --ignore-not-found
    }
    trap cleanup EXIT

    "${K6}" run \
      -o "experimental-prometheus-rw=${PROM_URL}/api/v1/write" \
      --env "GATEWAY_URL=${GATEWAY_URL}" \
      --env "K6_PROFILE=chaos" \
      --summary-export="${SUMMARY_FILE}" \
      "${ROOT}/loadtest/k6/scenarios/concert.js"
    ;;
  *)
    die "unknown round: ${ROUND}"
    ;;
esac

log "Round ${ROUND} done. summary=${SUMMARY_FILE}"
log "下一步：打开 http://localhost:3001 截取 Grafana 面板到 loadtest/report/round-${ROUND}-grafana-*.png"
