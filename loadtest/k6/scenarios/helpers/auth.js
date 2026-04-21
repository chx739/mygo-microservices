// 演唱会压测方案 § 3.Step 5：JWT 登录与 token 复用 helper。
//
// 职责：
//   1. 用 SharedArray 把 users.json 加载到所有 VU 的共享只读内存（k6 要求，避免每 VU copy）；
//   2. 按 role（rider / driver / attacker）划分账号子集，避免三角色互抢同一批 phone；
//   3. login(phone, password) 调 POST /auth/login，返回 access/refresh token；
//   4. VU 启动时登录一次，token 存到 VU-local 变量，主循环里拿来带 Bearer header 不做刷新。
//
// 为什么 VU-local 而不是 __ENV：k6 不同 VU 之间内存隔离，__ENV 是全局的，做不到 per-VU token。
// 为什么不刷新：方案 § 3.Step 0 已把 JWT_ACCESS_TTL_SECONDS 调到 1500（25min > 17min 场景时长）。

import http from "k6/http";
import { SharedArray } from "k6/data";
import { check, fail } from "k6";

// 读一次 users.json，所有 VU 共享同一份只读数组。
// 路径相对 k6 进程的 CWD（run.sh 把工作目录固定到仓库根，路径写绝对相对会更稳）。
const allUsers = new SharedArray("seed-users", function () {
  return JSON.parse(open("../../seed/users.json"));
});

// 按 role 过滤出三份子数组。rider 池 5000、driver 池 1000；
// attacker 压测里复用 rider 池（方案 § 3.Step 6 说明：attacker 必须带合法 rider token）。
export const riders = allUsers.filter((u) => u.role === "rider");
export const drivers = allUsers.filter((u) => u.role === "driver");
export const attackers = riders; // 显式别名，阅读 concert.js 更直观。

// BASE_URL 允许被 run.sh 通过 --env 覆盖，默认指向本机 Tilt port-forward。
export const BASE_URL = __ENV.GATEWAY_URL || "http://localhost:8081";

// pickUser 按 VU 编号取账号，保证同一 VU 每轮拿同一账号，避免 Redis 限流打架。
// idx = (__VU - 1) % pool.length，__VU 从 1 开始。
export function pickUser(pool) {
  if (!pool.length) {
    fail("empty user pool — 先跑 loadtest/k6/seed/seed.go");
  }
  const idx = (__VU - 1) % pool.length;
  return pool[idx];
}

// login 发一次 POST /auth/login，成功返回 {accessToken, refreshToken, role}，失败抛出。
// 不在这里做重试：登录阶段失败就是环境没起来，快速失败让 k6 早些报错。
export function login(user) {
  const res = http.post(
    `${BASE_URL}/auth/login`,
    JSON.stringify({ phone: user.phone, password: user.password }),
    {
      headers: { "Content-Type": "application/json" },
      tags: { endpoint: "auth" }, // 供 thresholds 里按 endpoint 分层阈值
    }
  );

  const ok = check(res, {
    "login status 200": (r) => r.status === 200,
    "has access_token": (r) => !!(r.json("data.access_token")),
  });
  if (!ok) {
    fail(`login failed for ${user.phone}: status=${res.status} body=${res.body}`);
  }

  return {
    accessToken: res.json("data.access_token"),
    refreshToken: res.json("data.refresh_token"),
    userID: user.userID || "", // seed 重跑的 409 路径没有 UUID，允许空
    role: user.role,
    phone: user.phone,
  };
}

// authHeaders 把 session 转成通用 Header 对象，concert.js 里用：
//   http.get(url, { headers: authHeaders(session) })
export function authHeaders(session) {
  return {
    Authorization: `Bearer ${session.accessToken}`,
    "Content-Type": "application/json",
  };
}
