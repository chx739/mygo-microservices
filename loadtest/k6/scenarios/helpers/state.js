// 演唱会压测方案 § 3.Step 5：跨 scenario 共享状态。
//
// 当前仅定义 tripID 池 —— driverFlow 需要从 riderFlow 创建出的 trip 里取
// tripID 去 accept / decline。k6 VU 之间不共享内存，所以用 k6 的 Counter
// 作为「已消费索引指针」，真实 tripID 池用 SharedArray 在脚本启动时加载。
//
// 真实落地在 Step 6 concert.js：riderFlow 创建 trip 后把 id 塞进 in-VU 队列
// 发给 WebSocket 订阅者，driverFlow 从 /ws/drivers 的推送里读，不走此模块。
// 此文件保留是为了未来补全生产-消费模型（超出 Phase 4 范围，先占位）。
//
// 当前只导出一个工具函数：randomHexID。用于 attackerFlow 生成 24 位十六进制的
// 伪造 tripID，走 /trip/:id 触发 Bloom miss（方案 § 3.Step 6）。

// randomHexID 返回长度为 len 的随机十六进制字符串，默认 24 位（mongo ObjectID 长度）。
// 用 k6 内置的 Math.random，不依赖任何扩展，避免增加 xk6 构建复杂度。
export function randomHexID(len = 24) {
  const chars = "0123456789abcdef";
  let s = "";
  for (let i = 0; i < len; i++) {
    s += chars.charAt(Math.floor(Math.random() * 16));
  }
  return s;
}
