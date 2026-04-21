// 演唱会压测方案 § 3.Step 7：SLO 阈值，拆 baseline / chaos 两档。
//
// concert.js 顶部按 __ENV.K6_PROFILE 选档：
//   - baseline：Round 2 全流量 + 无故障，对项目基本功能能力负责；
//   - chaos：Round 3 全流量 + 故障注入，允许 P99 略松、错误率上浮，
//           但 Bloom 拦截率不松动（防穿透是 hard requirement）。
//
// 阈值数字来自方案 § 7.3 SLO 表，数值不是拍脑袋 —— /trip/preview 的 P99 300ms
// 是因为其内部打 OSRM 网络请求，压缩到 300ms 已经很紧；/auth/* 的 P99 1s 是
// bcrypt 成本占大头，再压就要改 cost 参数（改业务代码，超出压测范围）。

// baselineThresholds：无故障基线。数字偏严格，拿来判定「静态能力达标」。
export const baselineThresholds = {
  "http_req_duration{endpoint:preview}": ["p(99)<300"],
  "http_req_duration{endpoint:start}": ["p(99)<500"],
  "http_req_duration{endpoint:auth}": ["p(99)<1000"],
  "http_req_failed{endpoint:preview}": ["rate<0.01"],
  "http_req_failed{endpoint:start}": ["rate<0.01"],
  // 自定义指标：rider 侧成功分配到司机的占比（在 concert.js 里用 Trend/Rate 定义）。
  trip_assigned_within_15s: ["rate>0.98"],
  // 攻击者路径的 Bloom 拦截率：404 视为「被拦」。
  attacker_blocked_by_bloom: ["rate>0.98"],
};

// chaosThresholds：带故障窗口。
//   - 延迟阈值整体放宽 40~60%（故障窗口真实打爆 P99 的天花板，放宽不等于放水）；
//   - /trip/start 错误率放到 4%：限流拦截 + 故障重启合法拒绝 都算 start 的「失败」，
//     如果卡 1% 会直接红灯误判；
//   - Bloom 拦截率阈值不动：即使链路挂了，Bloom miss 仍应该被 trip-service 早早拒绝，
//     不依赖 Redis/Mongo 的健康状态。
export const chaosThresholds = {
  "http_req_duration{endpoint:preview}": ["p(99)<500"],
  "http_req_duration{endpoint:start}": ["p(99)<800"],
  "http_req_duration{endpoint:auth}": ["p(99)<1500"],
  "http_req_failed{endpoint:preview}": ["rate<0.02"],
  "http_req_failed{endpoint:start}": ["rate<0.04"],
  trip_assigned_within_15s: ["rate>0.96"],
  attacker_blocked_by_bloom: ["rate>0.98"],
};

// pickThresholds 按 profile 返回阈值对象。concert.js 顶部只 import 此函数即可。
export function pickThresholds(profile) {
  return profile === "baseline" ? baselineThresholds : chaosThresholds;
}
