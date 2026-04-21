// 演唱会压测方案 § 3.Step 10：预热脚本，30s 低强度打流。
//
// 目的：让 api-gateway 建立 gRPC 连接池、Redis 连接池、Mongo driver 热起来，
// 避免正式压测第一分钟的数据被冷启动污染。预热流量走真实登录链路，
// 顺便确认 JWT/Redis 路径能通，Round 1 之前就发现问题。
//
// 运行：
//   ./k6 run -o 'experimental-prometheus-rw=http://localhost:9090/api/v1/write' \
//     loadtest/k6/scenarios/warmup.js

import { riderFlow } from "./concert.js";

export { riderFlow };

export const options = {
  scenarios: {
    warmup: {
      executor: "constant-vus",
      vus: 10,
      duration: "30s",
      exec: "riderFlow",
    },
  },
  // 预热阶段的失败不算违规，不设 thresholds。
};
