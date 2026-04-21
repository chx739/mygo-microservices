// 演唱会压测方案 § 3.Step 2.5：k6 → Prometheus remote_write 通路 Smoke 测试。
//
// 为什么要单独写一个 Smoke：压测主脚本 concert.js 在 Round 2/3 跑 17 分钟才能看到指标，
// 若通路断了（Prom 未启 remote-write-receiver、k6 扩展缺失、地址写错等）会浪费一整轮时间。
// 本脚本用 10 个 VU 跑 30 秒，目标是产出稳定的 http_reqs / iterations 指标流，
// 只要能在 Grafana 的 Prometheus 数据源里看到 `k6_http_reqs_total` 就算 PASS。
//
// 运行方式（由 loadtest/run.sh 包装，也可手跑）：
//   ./k6 run \
//     --out experimental-prometheus-rw=http://localhost:9090/api/v1/write \
//     loadtest/scripts/smoke_remote_write.js
//
// 前置：
//   1) xk6 build --with github.com/grafana/xk6-output-prometheus-remote 已生成 ./k6
//   2) kubectl port-forward svc/prometheus 9090:9090 已打开
//   3) Prometheus Deployment 启动参数含 --web.enable-remote-write-receiver

import http from "k6/http";
import { check, sleep } from "k6";

// 轻量配置：10 VU / 30s，产出足够抽样的 RED 指标，又不拖长 Smoke 耗时。
export const options = {
  vus: 10,
  duration: "30s",
  // Smoke 不设 thresholds：能跑完 + Prometheus 查得到指标 = PASS。
  // 真实压测的阈值见 loadtest/scripts/thresholds.js（方案 § 3.Step 7）。
};

// 可覆盖的靶点：默认打 Prometheus 自身 /-/ready，只为产生 HTTP 请求指标流；
// 不依赖业务服务起没起来，降低 Smoke 失败面。
const TARGET = __ENV.SMOKE_TARGET || "http://localhost:9090/-/ready";

export default function () {
  const res = http.get(TARGET);
  check(res, {
    "smoke target reachable": (r) => r.status === 200,
  });
  sleep(0.1);
}
