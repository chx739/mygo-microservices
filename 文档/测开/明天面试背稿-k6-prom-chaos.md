# 明天面试速背稿 - k6 / Prometheus / Chaos Mesh

> 目标：直击重点、避免露馅、口头讲得自然。每节配"口头实现表述"，按这段背就行。

---

# 一、k6（端到端压测）

## 一句话定位

> k6 是 Go 写的脚本式压测工具，JS 写 scenario，单进程多 goroutine 支持高并发，原生支持 WS 和多角色场景。

## 实现流程（5 步背）

| 步骤 | 做了什么 |
|---|---|
| ① 选工具 | k6 比 JMeter（XML/GUI 重）、Locust（GIL）、wrk（单 URL）适合多步骤业务流 + WS |
| ② 写场景 | `scenarios` 块配 rider / driver / attacker 三个 executor 并发跑 |
| ③ 写业务流 | 每个角色一个 default function，按真实业务流写多步骤 |
| ④ 编 SLO | `thresholds.js` 把 SLO 写成断言，没达标 exit 非 0 |
| ⑤ 推指标 | xk6 编译进 `xk6-output-prometheus-remote`，跑时 push 到 Prom |

## 口头实现表述（背这段！）

> "我用 k6 做端到端压测，scenarios 配了三个角色——rider 用 `ramping-vus` 阶梯爬坡 50 到 2000、driver 用 `constant-vus` 800 个长连接 WS、attacker 用 `constant-arrival-rate` 200 RPS 打伪造 ID 验证防御层。rider 的业务流是登录拿 token → 先 WS 连上 `/ws/riders`、在 open 回调里发 preview → start → 15 秒等 `driver_assigned` 消息。**这个顺序是踩坑出来的——必须先 WS 连上再发 start，否则 gateway 还没注册 conn，派单消息推不回来**。SLO 写在 `thresholds.js` 里，比如 `http_req_duration{endpoint:preview}: p(99)<300ms`、`attacker_blocked_by_bloom: rate>0.98`，k6 跑完不达标进程 exit 非 0，可以直接挂 PR 卡口当 CI 门禁。指标用 xk6 编译进 prometheus remote_write 扩展，跑时 push 给 Prometheus，和服务端的 pull 指标在同一个 Grafana 面板上对照看。一条命令 `./k6 run --summary-export=...json concert.js`，summary JSON 留底做多轮对比。"

## 高频追问速答

| 问 | 答 |
|---|---|
| **为什么 k6 不用 JMeter/Locust/wrk？** | JMeter 是 XML+GUI CI 不友好；Locust Python GIL 限并发；wrk 极快但只能打单 URL，**不能表达多步骤 + WS**。k6 单进程 goroutine 模型 + scenarios 多角色 + 原生 WS 是天生匹配。 |
| **k6 的 VU 和 iteration 区别？** | VU = 虚拟用户（并发数），iteration = 一个 VU 跑完一遍 default function。VU 复用，跑完接着跑下一轮。 |
| **threshold 是干嘛的？** | 把 SLO 写成断言，没达标 k6 进程 exit 非 0，**可直接挂 CI 门禁**。这是 k6 区别于 wrk 的关键。 |
| **遇到过什么坑？** | WS 升级 100% 失败——`trip_assigned` 永远 0%。最后 curl 单独打 `/ws/riders` 看到 400 不是 101，定位到 **metrics 中间件的 statusRecorder 没实现 `http.Hijacker` 接口**，gorilla 断言失败直接 400。8 行代码加个 `Hijack()` 方法透传底层 conn 修复。 |
| **怎么找到瓶颈的？** | k6 黑盒指标看到 P99 飙，配合服务端业务白盒指标对照——比如 `auth p99=15s`，是 bcrypt 慢哈希；`trip_assigned=0.74%`，是派单容量。**两边对照才能定位**。 |

---

# 二、Prometheus（监控）

## 一句话定位

> Prometheus 是 CNCF 出品的开源 TSDB + Pull 模型监控系统，PromQL 查询，K8s 生态事实标准。

## 5 组件 / 5 步流程（必背）

**5 组件**：Server（主进程）+ Exporters（第三方系统适配）+ Pushgateway（短命任务）+ Alertmanager（告警）+ Grafana（可视化）

**5 步流程**：
1. **暴露**：服务在 `/metrics` 暴露纯文本指标
2. **采集**：Prometheus 按 `scrape_interval` 定时 pull
3. **存储**：写入本地 TSDB（默认 2h 内存 + 块文件，长期靠 Thanos）
4. **查询**：PromQL 查指标
5. **告警**：周期评估 rules → Alertmanager 路由

## 4 种指标类型（必背）

| 类型 | 语义 | 我项目里用在哪 |
|---|---|---|
| **Counter** | 只增不减 | 请求总数、消费总数、Bloom 命中次数 |
| **Gauge** | 可增可减瞬时值 | DLQ 当前深度 |
| **Histogram** | 分桶统计 | HTTP 延迟分位 |
| **Summary** | 客户端预算分位 | **不用**——多副本分位不能聚合 |

**关键金句**："多副本必须用 Histogram 不能用 Summary——Histogram 在服务端存 bucket，Prometheus 可以 `histogram_quantile()` 跨副本算全局 P99；Summary 在客户端算分位，多实例聚合时分位数不能合并。"

## 口头实现表述（背这段！）

> "我用 prometheus client_golang 在 `shared/metrics` 包统一定义 8 个指标，4 个服务共用同一个 Register 函数，用 `sync.Once` 防重复注册避免单测 panic。所有指标加 `service` 这个 ConstLabel，Grafana 面板按 service 过滤。HTTP 自动埋点走 `Middleware`——内部包一层 `statusRecorder` 抓 status code 和延迟，**这个 wrapper 必须实现 `http.Hijacker` 接口，否则 WebSocket 升级 100% 失败**。`pathTemplate` 用白名单 + `strings.HasPrefix("/trip/")` 把 `/trip/<24位hex>` 折叠成 `/trip/:id`，**否则高基数 label 会让 Prometheus OOMKilled**，第一轮压测 Prometheus 真重启了 25 次才发现这个坑。每个服务 main.go 三行连贯：`metrics.Register("xxx") → mux.Handle("/metrics", metrics.Handler()) → metrics.Middleware(mux)`。Prometheus 用 K8s ConfigMap 配 `static_configs` 静态抓 4 个服务，同时开 `--web.enable-remote-write-receiver` 接 k6 推上来的压测指标——**压测客户端是短命进程 pull 抓不到，所以 push；服务端长期存活仍用 pull，两种模型按生命周期分工**。retention=2h，memory limit 从默认 512Mi 上调到 1Gi 防 OOM。"

## PromQL 5 个核心模式（记表达式骨架）

```promql
# 1. QPS
rate(http_requests_total[1m])

# 2. P99 延迟（多副本聚合关键写法）
histogram_quantile(0.99,
  sum by (le) (rate(http_request_duration_seconds_bucket[5m])))

# 3. 错误率
sum(rate(http_requests_total{status=~"5.."}[1m]))
  / sum(rate(http_requests_total[1m]))

# 4. DLQ 堆积（Gauge 直接用）
rabbitmq_dlq_depth

# 5. 告警 rule 写法（持续 5 分钟才触发，防抖）
rate(http_requests_total{status=~"5.."}[5m]) > 0.05  for: 5m
```

## 8 个业务指标（背三层分组）

| 层 | 指标 |
|---|---|
| **流量层** | `http_requests_total`（Counter）、`http_request_duration_seconds`（Histogram） |
| **消息层** | `rabbitmq_consumed_total`（Counter，result=ok/dlq/retry）、`rabbitmq_dlq_depth`（Gauge） |
| **并发控制层** | `trip_accept_lock_contention_total`、`bloom_filter_hits_total`、`rate_limit_rejections_total`、`idempotency_dedup_total`（都是 Counter） |

## 高频追问速答

| 问 | 答 |
|---|---|
| **Pull 和 Push 怎么选？** | Pull 服务发现简单 + 监控挂了不影响业务，所以服务端用 pull；短命进程（k6/cron）pull 抓不到所以 push。**按生命周期分工。** |
| **为什么不用 OpenTelemetry Metrics？** | 项目只有 trace 走 OTel，metrics 直用 prom client 更轻，少一层 SDK 转译。OTel Metrics 还是 beta 不如 prom client 稳。 |
| **高基数 label 怎么办？** | path 模板化（`/trip/<24hex>` → `/trip/:id`），错误详情走日志不走指标。第一轮我真踩到，Prom 重启 25 次。 |
| **怎么做长期存储？** | Prometheus 单实例上限几周，要长期看用 **Thanos / Cortex / Mimir** 接 S3。 |
| **告警怎么做？** | Prometheus Server 评估 PromQL rule → Alertmanager 去重/分组/路由到 Slack/PagerDuty。`for: 5m` 持续触发才告警防抖。 |

---

# 三、Chaos Mesh（只留 2 个场景）

## 先讲 RTO / RPO（必背）

| 缩写 | 全称 | 中文 | 一句话 |
|---|---|---|---|
| **RTO** | Recovery **Time** Objective | 恢复时间目标 | 故障到业务恢复的**时长上限** |
| **RPO** | Recovery **Point** Objective | 恢复点目标 | 故障可容忍的**数据丢失量上限** |

**金句**："RTO 是恢复多快，RPO 是丢多少。互联网业务一般 RTO < 5min、RPO < 1min；金融业务 RTO 可放宽但 RPO 必须 = 0。"

## 一句话定位

> Chaos Mesh 是 K8s 上的声明式故障注入框架，CRD（PodChaos/NetworkChaos/Workflow）编排可复现的故障，比 `kubectl delete pod` 强一个数量级。

## 通用流程（每个场景共用）

1. **写 CRD YAML**：定义 `kind`、`spec.action`、`spec.selector`、`spec.duration`
2. **`kubectl apply -f xxx.yaml`** 注入
3. **同时跑 k6 压测**做负载背景
4. **Grafana 加 annotation 竖线**标记注入点
5. **观察业务指标**跌破 SLO 时打 mark、恢复时打 mark，差值 = RTO
6. **跑完 `kubectl delete -f`** 清理

---

## 场景 1：PodKill driver-service

### YAML 骨架（背关键字段，对应真文件 `loadtest/chaos/pod-kill-driver.yaml`）

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: PodChaos
metadata:
  name: concert-pod-kill-driver
  namespace: chaos-mesh                # CRD 实验本身放 chaos-mesh ns
spec:
  action: pod-kill
  mode: one                            # 只杀一个；可选 all / fixed / fixed-percent
  selector:
    namespaces: ["default"]            # 被杀的 pod 在 default ns
    labelSelectors:
      app: driver-service
  duration: "60s"                      # 实验 CR 60s 后结束；pod-kill 是一次性动作，不影响 pod 重启
```

**关键字段背点**：
- `mode: one` 选一个目标——还有 `all`（全杀）、`fixed: 2`（指定数量）、`fixed-percent: 50`（百分比）
- `namespace: chaos-mesh` vs `selector.namespaces: ["default"]` 是**两个 ns**——CRD 装在 chaos-mesh，目标在 default
- `duration` 对 pod-kill 是"实验 CR 存活时间"，不是"pod 死多久"——pod 杀完 K8s 立刻重启

### 流程（按时间轴讲）

1. **T+0s**：apply YAML，Chaos Mesh kill 一个 driver-service pod
2. **T+0~T+30s**：K8s 检测 NotReady，新 pod 拉起；故障窗口内 rider 拉单的 `trip.event.created` 消息**在 RabbitMQ 队列堆积不丢**（持久化 + 未 ack）
3. **T+30s 左右**：新 pod 起来，重连 RabbitMQ，**重投消息**开始消费
4. **T+30s 后**：业务指标短暂下探后恢复

### 验证什么（断言）

- ✅ `rabbitmq_consumed_total` 速率短暂归零后追平堆积
- ✅ **`idempotency_dedup_total` 增量 > 0**——证明重投消息被识别为重复
- ✅ Mongo trip 表**无重复派单**（同一 tripID 没被两个 driver 都接到）
- ✅ `trip_assigned_rate` 下探后 1min 内恢复

### 预期 RTO / RPO

| 指标 | 数值 |
|---|---|
| **RTO** | < 60s（pod 启动 30s + 消费追平 10-20s） |
| **RPO** | **0**（所有消息都通过 RabbitMQ 重投处理，业务幂等保证不重复） |

### 口头表述（背这段！）

> "T+0 杀掉 driver-service 一个 pod 后，K8s 检测 NotReady 拉新 pod，故障 30 秒内 rider 拉单的消息**在 RabbitMQ 队列堆积但不丢**——因为消息是 persistent + 未 ack 状态。新 pod 起来重连 RabbitMQ，重投消息开始消费。**关键断言是 `idempotency_dedup_total` 有增量**，证明重投消息被我的幂等状态机识别为重复、跳过二次执行，**Mongo trip 表也没出现重复派单**。预期 RTO < 60s，RPO = 0。这个场景本质验证**消息可靠性 + 幂等接管**——RabbitMQ 当缓冲、幂等保证不重复派单，两层配合实现故障期间业务无感。"

### 高频追问

| 问 | 答 |
|---|---|
| **为什么 RPO = 0？** | RabbitMQ 消息是 persistent + 未 ack，pod 挂了消息不丢；新 pod 重投消费 + 业务幂等保证不重复处理。两层保障让 RPO = 0。 |
| **怎么测 RTO？** | Grafana 在故障注入点和恢复点打 annotation 竖线，业务指标（如 `rabbitmq_consumed_total` rate）跌破 SLO 那一刻打 mark，恢复回去那一刻再打一个，时间差就是 RTO。 |
| **副本数 = 1 时怎么办？** | RTO 会更长（要等新 pod 完全启动），但 RPO 仍然 = 0（消息在队列里）。生产建议 replicas >= 2 才能做到秒级 RTO。 |

---

## 场景 2：NetworkChaos delay redis 50ms

### YAML 骨架（背关键字段，对应真文件 `loadtest/chaos/network-delay-redis.yaml`）

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: concert-redis-delay
  namespace: chaos-mesh
spec:
  action: delay
  mode: one
  selector:
    namespaces: ["default"]
    labelSelectors:
      app: redis
  delay:
    latency: "50ms"                    # 平均延迟
    correlation: "25"                  # 相邻包延迟相关性 25%（值越大延迟越平稳）
    jitter: "10ms"                     # 抖动 ±10ms，模拟真实网络不规律
  duration: "60s"
```

**关键字段背点**：
- `action: delay` 还有 `loss`（丢包）、`duplicate`（重复包）、`corrupt`（损坏包）、`partition`（网络分区）四种
- `latency + jitter + correlation` 三件套**才接近真实网络**——纯 50ms 是理想模型，加 jitter 才模拟抖动
- `correlation` 越大延迟越平稳；为 0 时每个包独立随机抖动，比生产更随机

### 流程（按时间轴讲）

1. **T+0s**：apply YAML，Chaos Mesh 给 Redis pod 注入 50ms 出入向网络延迟
2. **T+0~T+60s**：**所有走 Redis 的路径延迟增加**——限流脚本（Lua）、接单锁（SETNX）、幂等状态机、Bloom 过滤器查询、L2 缓存全部受影响
3. **T+0~T+60s**：业务 P99 延迟整体抬升，但**不应该出错**——Redis client 默认 timeout 是 3 秒，远大于 50ms 延迟
4. **T+60s 后**：注入结束，延迟立刻恢复正常

### 验证什么（断言）

- ✅ start P99 上升但 < 800ms（chaos 档 SLO，比 baseline 500ms 放宽）
- ✅ start 错误率 < 4%（chaos 档）
- ✅ **不出现雪崩**——不应该因为限流脚本变慢导致请求堆积到 OOM
- ✅ Bloom 拦截率仍 > 98%（防御层不退化）

### 预期 RTO / RPO

| 指标 | 数值 |
|---|---|
| **RTO** | 注入结束**立即恢复**（这不是真"故障"恢复，是降级容忍测试） |
| **RPO** | **0**（没有数据丢失，只是慢） |

### 口头表述（背这段！）

> "T+0 给 Redis pod 注 50ms 网络延迟后，所有走 Redis 的路径——限流、接单锁、幂等、Bloom、L2 缓存——延迟都增加。**关键是不应该出错也不应该雪崩**：因为我用的 Redis client 默认 timeout 是 3 秒远大于 50ms 延迟，所以慢但不会失败。我会断言 start P99 在 chaos 档 SLO 800ms 以内、错误率小于 4%、Bloom 拦截率仍然超过 98%。注入结束立即恢复，所以 RTO ≈ 0，RPO = 0。这个场景验证的是**降级路径和防雪崩**——如果限流脚本依赖 Redis 而没有客户端超时，单点 Redis 抖动会把整个 api-gateway 拖垮，这是要主动测出来的。"

### 高频追问

| 问 | 答 |
|---|---|
| **为什么不模拟 Redis 挂掉？** | 全挂是更极端的故障，但 **50ms 延迟更现实**——生产网络抖动远比"全挂"常见，是测开应该重点覆盖的灰色场景。 |
| **如果客户端没设 timeout 会怎样？** | 请求会堆积——goroutine 阻塞在 Redis 调用上不释放，最终 OOM。**这就是要测出来的，是混沌的价值**。 |
| **50ms 这个数怎么定的？** | 参考生产 Redis 平均 RTT < 5ms，注入 50ms 相当于 10 倍延迟模拟"网络异常但未中断"。再大（200ms+）就接近"挂了"，意义不同。 |

---

## Chaos Mesh 收尾追问（必背）

| 问 | 答 |
|---|---|
| **为什么用 Chaos Mesh 不手动 `kubectl delete pod`？** | 手动 kill 不可复现、不能精确控时间窗口、不能注入网络延迟/丢包。Chaos Mesh 是声明式 CRD，可复现 + 可编排。 |
| **如果让你给新服务设计第一个混沌场景？** | **选 PodKill**——YAML 最简单、出图最快、不挑环境、验证最基础的"副本数 + K8s 自愈"假设。NetworkChaos 等更激进的渐进式叠加，**一口气全上踩坑概率高**。 |
| **跑过完整流程没？** | 诚实答："**场景设计 + YAML 已落地，完整跑通是后续 TODO**，目前用于演示稳定性测试的方法论和场景规划能力。如果让我现在跑，会先跑最简单的 PodKill 验证 K8s 自愈，再渐进叠加。" |

---

# 收尾：三连金句（被打断/收尾时一定要说）

1. **"k6 用 thresholds 把 SLO 当 CI 卡口，不调阈值掩盖瓶颈"**（测开度量观）
2. **"Prometheus 高基数 label 是头号杀手，path 必须模板化"**（真踩过的坑）
3. **"混沌的价值在主动暴露非预期故障，单测和压测都测不到"**（理念）

---

# 最后 3 行（被问还有什么补充时说）

- "实际上 6 个业务指标里只有 HTTP 两个走 middleware 自动埋点，其他 4 个（Bloom / Lock / 限流 / 幂等）目前在测试和压测里使用，**业务代码埋点是下一步要补的**。"
- "Round 3 完整混沌跑通是 TODO，目前用于演示场景设计能力。"
- "覆盖率门禁、k6 smoke 进 CI、chaos nightly 都是演进路线图，**先把基础 CI 跑稳，再渐进叠加**。"

**诚实是面试中的免死金牌——比硬装"全做过"安全 10 倍。**
