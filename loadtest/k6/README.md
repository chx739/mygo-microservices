# 演唱会压测 - k6 入口

本目录是演唱会散场压测方案（`演唱会压测实现方案.md`）的脚本层实现。
运行前先读方案 § 0 / § 1 / § 3。

## ⚠️ 必须用仓库根 `./k6`，不是 $PATH 的官方 k6

压测指标通过 `experimental-prometheus-rw` 推到 Prometheus，这是 xk6 扩展，
官方 k6 没有。构建方式：

```bash
go install go.k6.io/xk6/cmd/xk6@latest
cd /home/hx/workspace/code/mygo/mygo-microservices
xk6 build --with github.com/grafana/xk6-output-prometheus-remote
./k6 version        # 应含 xk6-output-prometheus-remote
```

## 目录

```
loadtest/k6/
├── scenarios/
│   ├── concert.js         主压测脚本（ramping 50→2000 / constant 800 / constant-rate 500）
│   ├── smoke.js           30s × 3 scenario，上线前校验三角色 flow 全通
│   ├── warmup.js          30s 低强度预热，让连接池/缓存热起来
│   ├── smoke_remote_write.js  Step 2.5 通路 smoke，仅打 Prometheus /-/ready
│   └── helpers/
│       ├── auth.js        SharedArray 读 users.json + login + Bearer header
│       ├── payload.js     场馆坐标 + preview/start 请求体
│       └── state.js       状态工具（当前仅 randomHexID）
├── seed/
│   ├── seed.go            种子数据脚本（5000 rider + 1000 driver）
│   └── seed_test.go       三条单元用例
└── thresholds.js          baseline / chaos 双档 SLO 阈值
```

## 运行

一键：
```bash
./scripts/loadtest_run.sh --round 1   # Dry Run
./scripts/loadtest_run.sh --round 2   # Baseline
./scripts/loadtest_run.sh --round 3   # Full Chaos
```

单个脚本：
```bash
./k6 run \
  -o "experimental-prometheus-rw=http://localhost:9090/api/v1/write" \
  --env GATEWAY_URL=http://localhost:8081 \
  --env K6_PROFILE=baseline \
  loadtest/k6/scenarios/concert.js
```
