// 演唱会压测方案 § 3.Step 7：smoke 脚本，30s × 3 阶段验证三角色 flow。
//
// 为什么不直接用 --stage 覆盖 concert.js：
//   concert.js 里有三个不同 executor（ramping-vus / constant-vus / constant-arrival-rate），
//   k6 的 --stage 只能覆盖默认 executor，drivers / attackers 两条路径不会被验证。
//   因此 smoke.js 独立定义极短 scenarios，复用 concert.js 的 flow 函数。
//
// 运行：
//   ./k6 run loadtest/k6/scenarios/smoke.js
//   预期：≤ 90 秒跑完，三角色 flow 各跑 ≥ 1 次，无脚本错误。

import { riderFlow, driverFlow, attackerFlow } from "./concert.js";

export { riderFlow, driverFlow, attackerFlow };

export const options = {
  scenarios: {
    riders: {
      executor: "constant-vus",
      vus: 3,
      duration: "30s",
      exec: "riderFlow",
    },
    drivers: {
      executor: "constant-vus",
      vus: 2,
      duration: "30s",
      exec: "driverFlow",
      startTime: "5s",
    },
    attackers: {
      executor: "constant-arrival-rate",
      rate: 5,
      timeUnit: "1s",
      duration: "30s",
      preAllocatedVUs: 3,
      exec: "attackerFlow",
    },
  },
  // Smoke 不设 thresholds —— 只求「脚本能跑通、三角色都执行、无 runtime error」。
  // 正式阈值在 concert.js 的 baseline/chaos profile 里。
};
