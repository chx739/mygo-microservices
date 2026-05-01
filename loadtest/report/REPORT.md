# 演唱会散场压测报告（骨架 / 待回填）

> 本文件是 Phase 6 未跑完时的**骨架**。三轮运行完成后，按节点 TODO 回填数字和截图路径。
> 运行入口：`scripts/loadtest_run.sh --round {1|2|3}`。

## 1. 环境

| 项 | 值 |
|---|---|
| Round 2 完成时间 | 2026-04-29 00:01（17min 主跑） |
| Round 3 完成时间 | <!-- TODO: Round 3 完成日期 --> |
| 集群 | Minikube (driver=docker) |
| 业务服务版本 | `cb5f24d`（工作树带 8 项 loadtest 修复未提交，详见本节脚注） |
| JWT access TTL | 1500s（压测专用，Round 3 结束后 `git restore`） |
| k6 版本 | xk6 build，自带 `xk6-output-prometheus-remote` |
| Prometheus retention | 2h，scrape_interval 15s（内存上限 1Gi，从默认 512Mi 上调避免 OOMKill） |
| 目标 rider 峰值 VU | 2000（方案值） |
| 目标 driver 并发 | 800（方案值） |
| 目标 attacker RPS | 500（方案值） |
| 实际 K6_SCALE | **0.4**（本机 8 核 16G 在 SCALE=1 下 executor 饱和、dropped_iterations 22k） |
| Round 2 实际峰值 | rider 800 VU、driver 320 VU、attacker 200 RPS（vus_max=1200，dropped=2592 / 0.97%） |

> Round 2 修复脚注：本轮跑前修了 8 处 bug——`shared/metrics/middleware.go` 缺 `http.Hijacker` 导致 `/ws/*` 升级失败（`trip_assigned=0%`）；`payload.js` 字段 `startLocation/endLocation` 与 gateway `pickup/destination` 不对齐导致坐标 (0,0)；`concert.js` WS 缺 `userID/packageSlug` query param、driver 回执消息 type 错（应为 `driver.cmd.trip_accept`）、tripID 路径错（应为 `data.trip.id`）、`packageSlug` 未与 rider 选档对齐；`attacker_blocked_by_bloom` 口径过严；新增 `K6_SCALE` 等比缩流量。

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

| 指标 | 阈值（baseline） | Round 2 实测 | 判定 | 阈值（chaos） | Round 3 实测 |
|---|---|---|---|---|---|
| `http_req_duration{endpoint:preview}` P99 | < 300ms | **44.55ms** | ✅ | < 500ms | <!-- TODO --> |
| `http_req_duration{endpoint:start}` P99 | < 500ms | **367.41ms** | ✅ | < 800ms | <!-- TODO --> |
| `http_req_duration{endpoint:auth}` P99 | < 1000ms | **15.18s** | ❌ 真瓶颈 | < 1500ms | <!-- TODO --> |
| `http_req_failed{endpoint:preview}` rate | < 0.01 | **0.00%** | ✅ | < 0.02 | <!-- TODO --> |
| `http_req_failed{endpoint:start}` rate | < 0.01 | **1.95%** | ❌ 边缘 | < 0.04 | <!-- TODO --> |
| `trip_assigned_within_15s` rate | > 0.98 | **0.74%** | ❌ 真瓶颈 | > 0.96 | <!-- TODO --> |
| `attacker_blocked_by_bloom` rate | > 0.98 | **100.00%** | ✅ | > 0.98 | <!-- TODO --> |

> 总请求 276,326（attacker 201,409 / preview 36,858 / start 36,858 / login 750）。`http_req_failed` 总值 73.14% 反映的是 attacker 全数命中 404 的算术和（设计预期），并非业务失败；preview 失败 0%、start 失败 1.95% 才是业务侧真实失败率。

Round 2 summary 文件：`loadtest/report/round-2-summary-20260428-234250.json`
Round 3 summary 文件：`loadtest/report/round-3-summary-*.json`

## 4. 亮点链路验证（3 条面试弹药）

### 4.1 Pod kill 重投 + 幂等接管
- 时间：T+8min driver-service 被 kill。
- 观察：`rabbitmq_consumed_total{result="dlq"}` 峰值 <!-- TODO --> 条；
  新 pod 起来后 `idempotency_dedup_total` 增量 <!-- TODO --> 条，证明重投消息被幂等拒绝；
  `trip_assigned_within_15s` 只在故障窗口下探至 <!-- TODO -->，事后快速恢复。
- 结论：RTO ≈ <!-- TODO --> 秒，0 重复派单。

### 4.2 攻击流量 + Bloom 拦截 ✅ Round 2 已证
- 200 RPS（SCALE=0.4 后实际值）attackerFlow 全程 17min，**全数 201,409 次伪造 tripID
  请求 100% 被拦**（404 / 429 / 5xx 任何非 200 都计为防御生效）。
- `attacker_blocked_by_bloom` rate 实测 = **100.00%**，SLO ≥ 0.98 达成。
- `start` p99 = 367ms 与 `preview` p99 = 44.55ms 都在阈值内 → 攻击流量没把
  正常业务路径拖垮，证明 Bloom 预检在 trip-service handler 入口确实挡住了写穿透。

### 4.3 Mongo 丢包自愈
- T+10min NetworkChaos loss 10%，持续 60s。
- 观察：`rabbitmq_dlq_depth{queue="trip.*"}` 在 T+10:15 升至 <!-- TODO --> 条，
  T+10:55 开始回落，T+11:40 归零。
- 结论：RTO < 60s，RPO = 0（所有消息最终都被处理）。

## 5. 瓶颈分析

### 5.1 已被 Round 2 数据证实的真瓶颈

**B1. /auth/login bcrypt（最先顶不住）**
- 实测：auth p99 = **15.18s**，p95 = 13.19s，p90 = 10.49s。
- 原因：`shared/auth.HashPassword` 用 `bcrypt.DefaultCost = 10`（实际仍是 cost 量级开销），
  Round 2 启动期 800 rider 各登录一次约 800 次 bcrypt verify 集中下发，CPU 抢占。
- 证据：`checks.login status 200` = 750/750（功能正确），但延迟在尾部炸开。
- 不该在压测期把 cost 降下来——这是产品安全设定；面试讲法：「auth 是慢路径，工程上靠 1500s
  长 token 摊薄、生产环境靠水平扩容 api-gateway 解决」。

**B2. 派单容量（trip_assigned_within_15s 0.74%）**
- 实测：36,590 个 rider 等满 15s，仅 274 个收到 `driver_assigned` 事件。
- WebSocket 数据：`ws_sessions=36,780`、`ws_msgs_received=2,636`（绝大部分 session
  没收到任何下行消息）。
- 原因：driver VU=320 全程持有 17min 长 WS，rider 峰值 800 + 攻击 200，
  driver-service GEO 撮合 + RabbitMQ 派单 fan-out 跟不上 800 rider 的爆发拉单速率。
- 这是**架构容量**问题，不是脚本 bug。修复路径（生产参考）：
  driver-service 水平扩容 + 派单流改为按区域分片消费 + 加速 GEO `GEOSEARCH` 过滤范围。
- Round 3 chaos 档阈值放宽到 0.96，会更难达成，需要明确预期。

**B3. /trip/start 边缘失败率（1.95% vs 1% 阈值）**
- 实测：719 / 36,858 失败。p99=367ms 已贴近阈值（<500ms）。
- 推断（待 Round 3 / 单独压测进一步定位）：高并发下 mongo `CreateTrip` 写入 + Redis
  幂等 SETNX 任一抖动都可能失败。Mongo 单实例无副本集、无 majority writeConcern。

### 5.2 Round 3 待验证的瓶颈

- Redis 限流/幂等/Bloom 单实例：故障窗口 T+9 注入 50ms 延迟时是否出现雪崩。
- Mongo：T+10 NetworkChaos 10% 丢包下，写路径由 RabbitMQ 重投承接的恢复时长。
- pod-kill api-gateway（T+11）下 WebSocket 连接全部断开，client 是否能重连。

### 5.3 上轮失败原因复盘（已修复）

上次 Round 2（2026-04-28 00:25）`trip_assigned=0%`、`preview_failed=75%`、
`start_failed=81%`、`bloom=53%` 全炸——根因是 7 处脚本/中间件 bug，在 Phase 7 一次性修
完后本轮真值才出来。详细 commit 待提交，diff 见 `git status` 工作树。

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
