#!/usr/bin/env bash
# 演唱会压测方案 § 3.Step 3：装 Chaos Mesh（Helm + Minikube 参数）。
#
# 职责：一条命令把 Chaos Mesh v2.6 装到 minikube 集群的 chaos-mesh 命名空间，
# 带 Minikube runtime 专用参数（方案 § 3.Step 3 四件套）。装完自检 Pod Ready。
#
# 为什么要固定参数：Minikube 默认 containerd socket 路径与 K8s 生产不同，
# 不加 `chaosDaemon.runtime=containerd` 和 `socketPath` Chaos Daemon 会起不来。
# 这四个参数就是踩过一次坑后总结的必选项。

set -euo pipefail

NAMESPACE="${NAMESPACE:-chaos-mesh}"
CHART_VERSION="${CHART_VERSION:-2.6.3}"

log() { printf '[chaos-install %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { printf '[chaos-install FAIL] %s\n' "$*" >&2; exit 1; }

# 前置检查。
command -v helm    >/dev/null 2>&1 || die "需要 helm CLI，参考 https://helm.sh/docs/intro/install/"
command -v kubectl >/dev/null 2>&1 || die "需要 kubectl"

# 仓库与命名空间。
log "确保 chaos-mesh helm repo 存在并更新"
helm repo add chaos-mesh https://charts.chaos-mesh.org 2>/dev/null || true
helm repo update chaos-mesh

log "创建命名空间 ${NAMESPACE}（已存在则忽略）"
kubectl create namespace "${NAMESPACE}" 2>/dev/null || true

# Minikube runtime：containerd + 默认 socket。
# 如果你的 Minikube 用 docker driver，请改 socketPath=/var/run/docker.sock
# 并去掉 containerd 相关参数；方案 § 3.Step 3 的默认假设是 containerd 运行时。
log "helm upgrade --install chaos-mesh (version=${CHART_VERSION})"
helm upgrade --install chaos-mesh chaos-mesh/chaos-mesh \
  --namespace "${NAMESPACE}" \
  --version "${CHART_VERSION}" \
  --set chaosDaemon.runtime=containerd \
  --set chaosDaemon.socketPath=/run/containerd/containerd.sock \
  --set dashboard.create=true \
  --set dashboard.securityMode=false

log "等待 chaos-mesh 组件就绪（最多 180s）"
kubectl -n "${NAMESPACE}" rollout status deploy/chaos-controller-manager --timeout=180s
kubectl -n "${NAMESPACE}" rollout status deploy/chaos-dashboard --timeout=180s
kubectl -n "${NAMESPACE}" rollout status ds/chaos-daemon --timeout=180s || {
  die "chaos-daemon DaemonSet 未就绪，常见原因是 runtime/socketPath 不匹配，检查 kubectl -n ${NAMESPACE} describe pod -l app.kubernetes.io/component=chaos-daemon"
}

log "PASS：Chaos Mesh 已就绪。访问 Dashboard："
log "  kubectl -n ${NAMESPACE} port-forward svc/chaos-dashboard 2333:2333"
log "  浏览器打开 http://localhost:2333"
