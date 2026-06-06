# CI 设计说明（学习+背诵稿）

> 配套文件：`.github/workflows/ci.yml`（真文件，可指给面试官看）
> 上层项目说明：`./项目说明.md` §7

---

## 一句话定位

> 这套 CI 是 PR 卡口的最小可行版，两个 job 并行：**lint**（gofmt + vet）和 **test**（-race -cover），test 用 service container 起真实 mongodb 跑集成测试。**强调"可演进"**——先把基础跑通，后续按需叠加 golangci-lint / k6 smoke / chaos nightly。

---

## 设计原则（5 条，每条 1 句）

1. **快失败**：lint 单独成 job 且不依赖其他 job，失败立刻断，不浪费 test 算力（test job `needs: lint` 串行依赖）
2. **真实依赖优先**：mongodb 用 service container 不用 mock——mongo-driver mock 容易和真实行为偏离（索引、CAS、writeConcern）
3. **并发安全是底线**：所有测试 `-race` 跑，否则项目里的接单锁、消息幂等、缓存击穿这些并发逻辑等于裸奔
4. **可演进**：先跑通最小集合，后续叠加 golangci-lint / 覆盖率门禁 / k6 smoke / 安全扫描，不一次到位
5. **Artifact 可追溯**：coverage.out 作为 artifact 上传，PR 评审时可下载本地用 `go tool cover -html=coverage.out` 看红绿

---

## 工作流结构（讲给面试官听）

```
on: push (main) + pull_request
   │
   ├─ Job: lint                          (~30s)
   │   ├─ checkout
   │   ├─ setup-go (cache: true)
   │   ├─ gofmt -l . → 有 diff 就 exit 1
   │   └─ go vet ./...
   │
   └─ Job: test                          (needs: lint, ~3-5min)
       ├─ services: mongodb:6 (with health check)
       ├─ checkout
       ├─ setup-go (cache: true)
       ├─ go build ./...
       ├─ go test -race -cover -coverprofile=coverage.out ./...
       ├─ go tool cover -func=coverage.out | tail -1
       └─ upload-artifact coverage-report
```

---

## 选型对比（面试官会追问）

### Q：为什么 GitHub Actions 不用 GitLab CI / Jenkins / Drone？

- **GitHub Actions**（选它）：和 GitHub 仓库零集成、yaml 写在仓库里、service container 配置简单、第三方 action 生态最全
- GitLab CI：和 GitLab 仓库强绑定，跨平台移植需要改
- Jenkins：要自建 master + agent，运维成本高
- Drone：轻量但生态小

一句话："仓库托管在 GitHub，最低配置成本就用原生 Actions。"

### Q：为什么 mongo 用 service container 不用 testcontainers-go？

| 维度 | service container | testcontainers-go |
|---|---|---|
| 启动位置 | CI runner 上 docker run | 测试代码内 docker run |
| 测试速度 | 每个 job 启 1 次（jobs 共享） | 每个测试可独立启 |
| 配置 | yaml 几行 | Go 代码每个测试写 |
| 本地复用 | 不能（仅 CI） | 能（本地也能跑） |

- 我选 service container 是因为**简单 + CI 专用**。本地跑测试时开发者已经在 tilt 里有 mongodb，不需要 testcontainers
- testcontainers-go 适合"测试要起多个临时容器、容器配置在测试代码里动态决定"的场景。本项目目前用不上。

### Q：为什么没用 golangci-lint，只用 gofmt + vet？

- **最简版**先保证跑通：第一次写 CI 容易在 lint 严格度上和团队风格起冲突
- `gofmt + vet` 是 Go 官方工具，零争议
- **下一步**：加 golangci-lint 跑 `errcheck / staticcheck / ineffassign / unused / gosimple` 这 5 个最稳的 linter，等达成共识再加更激进的（如 `lll` 行长、`funlen` 函数长）
- 一句话："linter 是渐进引入的工程文化问题，不是技术问题。"

### Q：为什么没有 codecov 集成 / 覆盖率门禁？

- **不卡的原因**：现阶段覆盖率不到 70%，先卡会让 PR 推不动
- **演进路径**：先 report → 跑几周看真实分布 → 给关键模块（auth / 接单锁 / 幂等）设 ≥85% 阈值 → 全局设 ≥70%
- 直接上 codecov 还要配 token、配置 PR 评论格式，本轮不做

### Q：为什么没跑 k6 smoke？

- k6 smoke 跑 30 秒不算长，但要起完整集群（kind / minikube）才能跑通，CI 复杂度上一个台阶
- **演进路径**：等基础 CI 稳定 + 集群启停脚本（`scripts/loadtest_run.sh`）成熟后，加一个 nightly job 跑 smoke
- 现阶段 k6 smoke 靠开发者本地跑 + 提交前手动验证

### Q：为什么没跑安全扫描（gosec / dependency check）？

- **依赖扫描**：GitHub 自带 Dependabot 已经覆盖，不用单独 action
- **gosec**：可以加，下一步叠加。本轮聚焦"跑得绿"，不引入会大量误报的工具

---

## 关键 yaml 决策点详解

### `cache: true`（setup-go）

```yaml
- uses: actions/setup-go@v5
  with:
    go-version: '1.24'
    cache: true
```

- 默认按 `go.sum` hash 做 cache key，命中时直接复用 `~/go/pkg/mod` 和 `~/.cache/go-build`
- 首次 cold cache 跑 ~3min，命中后 ~30s。**对开发者体感影响巨大**

### MongoDB health check

```yaml
services:
  mongodb:
    image: mongo:6
    ports:
      - 27017:27017
    options: >-
      --health-cmd "mongosh --quiet --eval 'db.adminCommand({ping:1})'"
      --health-interval 10s
      --health-timeout 5s
      --health-retries 5
```

- 必须有 health check，不然 service container 可能还没 ready test 已经开跑，连接拒绝
- `mongo:6` 自带 mongosh，不用额外装；`mongo:4` 时代要用 `mongo --eval`
- 健康检查 5 次 × 10s = 50s 兜底，足够 mongo 起来

### `needs: lint`（test job 依赖 lint job）

```yaml
test:
  needs: lint
```

- 串行而不是并行：lint 失败就不跑 test，省 5min 算力
- 反方观点："lint 和 test 应该并行，反正都得跑"——也有道理，但我倾向 fail-fast 节约 GitHub Actions 免费额度

### `if: always()`（覆盖率上传）

```yaml
- name: Upload coverage artifact
  if: always()
  uses: actions/upload-artifact@v4
```

- 不管前面成败都上传——测试失败时的覆盖率反而最有价值（能看到哪些代码没跑到，可能是失败原因）

---

## 演进路线图（讲完最小版顺势抛出，体现深度）

| 阶段 | 时间 | 加什么 | 收益 |
|---|---|---|---|
| **已落地** | 现在 | gofmt + vet + race + cover | PR 卡口最小集合 |
| 短期 | 1 周 | golangci-lint（5 个稳健 linter） | 静态分析更深 |
| 短期 | 1 周 | 关键模块覆盖率门禁 ≥85% | 关键路径质量保证 |
| 中期 | 1 月 | k6 smoke 30s 作为 PR check | 端到端冒烟兜底 |
| 中期 | 1 月 | docker buildx 多平台构建 + 镜像 push | 出真实部署制品 |
| 长期 | 季度 | chaos-mesh nightly job | 稳定性回归保护 |
| 长期 | 季度 | gosec + Trivy 容器扫描 | 安全卡口 |

---

## 常见追问速答

**Q：CI 跑挂了，开发者怎么本地复现？**
- yaml 里所有命令都是本地可执行：`gofmt -l .`、`go vet ./...`、`go test -race -cover ./...`
- 开发者本地跑通这三个就基本和 CI 一致

**Q：测试 flaky 怎么办？**
- 三招：
  1. 找根因——80% 的 flaky 是测试依赖时序（sleep、随机数据），改成 deterministic
  2. 隔离运行——把 flaky 测试单独 build tag，CI 里独立 job 跑且允许重试 3 次
  3. 长期看板——记录每个测试的失败率，超过 1% 的测试列入红榜限期修复
- 我项目目前没踩到 flaky（因为并发测试用了 ready chan + atomic 而不是 sleep）

**Q：CI 时长太长怎么优化？**
- 三招：
  1. **并行化**——按包 / 按目录拆 matrix
  2. **缓存**——setup-go 的 `cache: true` 已开
  3. **增量测试**——只跑变更影响的包（`go list -deps` 反向分析）
- 现在 ~3-5min 还能接受，超过 10min 再优化

**Q：你的 CI 怎么对接通知？**
- 默认 GitHub Actions 在 PR check 上展示绿/红
- 进一步：失败时 GitHub bot 自动 comment，并把覆盖率 diff 贴出来
- 团队内可配 Slack webhook，主干失败立刻 @oncall

---

## 背诵优先级

1. **必背**：一句话定位 + 5 条设计原则 + 工作流 ASCII 图
2. **重点**：service container vs testcontainers 对比 + 为什么没用 golangci-lint
3. **加分**：演进路线图（让面试官知道你想得远，不是"做了什么是什么"）
