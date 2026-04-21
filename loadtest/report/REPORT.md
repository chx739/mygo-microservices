# 演唱会散场压测报告（骨架 / 待回填）

> 本文件是 Phase 6 未跑完时的**骨架**。三轮运行完成后，按节点 TODO 回填数字和截图路径。
> 运行入口：`scripts/loadtest_run.sh --round {1|2|3}`。

## 1. 环境

| 项 | 值 |
|---|---|
| 压测日期 | <!-- TODO: Round 3 完成日期 --> |
| 集群 | Minikube (driver=docker) |
| 业务服务版本 | `git rev-parse --short HEAD` = <!-- TODO 回填 --> |
| JWT access TTL | 1500s（压测专用，Round 3 结束后 `git restore`） |
| k6 版本 | xk6 build，自带 `xk6-output-prometheus-remote` |
| Prometheus retention | 2h，scrape_interval 15s |
| 目标 rider 峰值 VU | 2000 |
| 目标 driver 并发 | 800 |
| 目标 attacker RPS | 500 |

## 2. 场景时间线

```
T+0m      rider ramping 50 → 2000
T+1m      drivers 800 VU 上线
T+3m      到达峰值，稳态 10min
T+8m      [Chaos] pod-kill driver-service
T+9m      [Chaos] NetworkChaos delay redis 50ms
T+10m     [Chaos] NetworkChaos loss mongodb 10%
T+11m     [Chaos] pod-kill api-gateway
T+13m     rider 回落 2000 → 300
T+15m     rider 恢复 300 → 50，Workflow 结束观察 5min
T+17m     脚本结束
```

## 3. SLO 达成（Round 2 vs Round 3 对比）

| 指标 | 阈值（baseline） | Round 2 实测 | 阈值（chaos） | Round 3 实测 |
|---|---|---|---|---|
| `http_req_duration{endpoint:preview}` P99 | < 300ms | <!-- TODO --> | < 500ms | <!-- TODO --> |
| `http_req_duration{endpoint:start}` P99 | < 500ms | <!-- TODO --> | < 800ms | <!-- TODO --> |
| `http_req_duration{endpoint:auth}` P99 | < 1000ms | <!-- TODO --> | < 1500ms | <!-- TODO --> |
| `http_req_failed{endpoint:preview}` rate | < 0.01 | <!-- TODO --> | < 0.02 | <!-- TODO --> |
| `http_req_failed{endpoint:start}` rate | < 0.01 | <!-- TODO --> | < 0.04 | <!-- TODO --> |
| `trip_assigned_within_15s` rate | > 0.98 | <!-- TODO --> | > 0.96 | <!-- TODO --> |
| `attacker_blocked_by_bloom` rate | > 0.98 | <!-- TODO --> | > 0.98 | <!-- TODO --> |

Round 2 summary 文件：`loadtest/report/round-2-summary-*.json`
Round 3 summary 文件：`loadtest/report/round-3-summary-*.json`

## 4. 亮点链路验证（3 条面试弹药）

### 4.1 Pod kill 重投 + 幂等接管
- 时间：T+8min driver-service 被 kill。
- 观察：`rabbitmq_consumed_total{result="dlq"}` 峰值 <!-- TODO --> 条；
  新 pod 起来后 `idempotency_dedup_total` 增量 <!-- TODO --> 条，证明重投消息被幂等拒绝；
  `trip_assigned_within_15s` 只在故障窗口下探至 <!-- TODO -->，事后快速恢复。
- 结论：RTO ≈ <!-- TODO --> 秒，0 重复派单。

### 4.2 攻击流量 + Bloom 拦截
- 500 RPS attackerFlow 全程持续 17min，`bloom_filter_hits_total{result="miss_rejected"}`
  稳定在 <!-- TODO --> RPS；Mongo QPS 曲线与 Round 1 Dry Run 基线对齐（无可见上浮）。
- `attacker_blocked_by_bloom` rate 实测 <!-- TODO -->，SLO 要求 ≥ 0.98 达成。

### 4.3 Mongo 丢包自愈
- T+10min NetworkChaos loss 10%，持续 60s。
- 观察：`rabbitmq_dlq_depth{queue="trip.*"}` 在 T+10:15 升至 <!-- TODO --> 条，
  T+10:55 开始回落，T+11:40 归零。
- 结论：RTO < 60s，RPO = 0（所有消息最终都被处理）。

## 5. 瓶颈分析

- 预期首先顶不住的环节：bcrypt（/auth/login P99 占用大头，方案已把 TTL 拉长避开频繁登录）。
- 次预期瓶颈：Redis（限流/幂等/Bloom 全部走单实例 Redis），丢包 + 延迟叠加时是否出现雪崩，
  需看 Round 3 `http_req_duration{endpoint:start}` 的峰值分布。
- Mongo：通过 Bloom + cache-aside 预期读路径压力小，写路径在 pod kill 后由 RabbitMQ 重投承接。

<!-- TODO: 回填 Round 3 数据后写实际瓶颈位置 + 证据（面板截图时间点） -->

## 6. 截图（附 3 张）

| # | 文件 | 覆盖面板 Row |
|---|---|---|
| 1 | `loadtest/report/round-3-grafana-overview.png`    | Row 1 流量 + Row 4 资源 |
| 2 | `loadtest/report/round-3-grafana-messaging.png`   | Row 2 消息系统 |
| 3 | `loadtest/report/round-3-grafana-concurrency.png` | Row 3 并发控制（含 Bloom / 锁 / 限流） |

截图要求：
- 时间窗口覆盖完整 17min 压测 + 5min 恢复观察。
- 故障 annotation 竖线可见（T+8/9/10/11 各一条）。
- 注释框（Grafana → panel → edit → description）保留方案 § 3.Step 9 的面板说明。

## 7. 压测后清理

Round 3 结束后必须执行：

```bash
git restore infra/development/k8s/app-config.yaml   # TTL 1500 → 900
kubectl apply -f infra/development/k8s/app-config.yaml
kubectl rollout restart deploy/api-gateway
kubectl delete -f loadtest/chaos/schedule.yaml -n chaos-mesh --ignore-not-found
```

清理完成后在此处打勾：

- [ ] JWT_ACCESS_TTL_SECONDS 已恢复 900
- [ ] chaos-mesh workflow 已删除
- [ ] `git status` 只剩预期的 loadtest/ + shared/metrics/ + 4 个 deployment + Tiltfile + proto/+ 服务 main.go 的改动
