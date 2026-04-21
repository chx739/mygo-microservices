// 演唱会压测方案 § 3.Step 6：主压测脚本，17 分钟跑完「基线→爬升→峰值→故障→恢复」。
//
// 三 scenario 并发：
//   - riders    (ramping-vus, 50→2000→50)：乘客 preview + start + 等分配；
//   - drivers   (constant-vus 800)：司机 WebSocket 监听 trip_request，按概率 accept/decline；
//   - attackers (constant-arrival-rate 500 rps)：带合法 rider token，GET /trip/<伪造id>，
//                触发 Bloom miss 指标。
//
// 故障编排不由脚本触发 —— 见方案 § 3.Step 8 的 Chaos Workflow，
// 脚本只负责产出业务流量，故障注入由 chaos-mesh 按 T+8m/9m/10m/11m 打入。
//
// 自定义指标：
//   - trip_assigned_within_15s (Rate)：rider 15s 内被分配到司机的比例（SLO 指标）；
//   - attacker_blocked_by_bloom (Rate)：attacker 请求返回 404 的比例（Bloom 拦截率）。

import http from "k6/http";
import ws from "k6/ws";
import { check, sleep } from "k6";
import { Rate } from "k6/metrics";

import {
  BASE_URL,
  riders,
  drivers,
  attackers,
  pickUser,
  login,
  authHeaders,
} from "./helpers/auth.js";
import { buildPreviewRequest, buildTripStartRequest } from "./helpers/payload.js";
import { randomHexID } from "./helpers/state.js";
import { pickThresholds } from "../thresholds.js";

// 自定义指标。Rate 比 Counter 更适合做 SLO 阈值（直接表达百分比）。
const tripAssignedWithin15s = new Rate("trip_assigned_within_15s");
const attackerBlockedByBloom = new Rate("attacker_blocked_by_bloom");

// Profile 切换：默认 chaos，Round 2 run 时 --env K6_PROFILE=baseline。
const profile = __ENV.K6_PROFILE || "chaos";

export const options = {
  scenarios: {
    riders: {
      executor: "ramping-vus",
      startVUs: 50,
      stages: [
        { duration: "1m", target: 50 },    // 基线
        { duration: "2m", target: 2000 },  // 爬升
        { duration: "10m", target: 2000 }, // 峰值（Chaos Workflow 在此窗口内注入）
        { duration: "2m", target: 300 },   // 回落
        { duration: "2m", target: 50 },    // 恢复
      ],
      exec: "riderFlow",
      gracefulRampDown: "10s",
    },
    drivers: {
      executor: "constant-vus",
      vus: 800,
      duration: "17m",
      exec: "driverFlow",
      startTime: "30s", // 等 rider 有订单再让 driver 就位，避免空跑
    },
    attackers: {
      executor: "constant-arrival-rate",
      rate: 500,
      timeUnit: "1s",
      duration: "17m",
      preAllocatedVUs: 100,
      maxVUs: 200,
      exec: "attackerFlow",
    },
  },
  thresholds: pickThresholds(profile),
  // summary 里保留自定义指标，REPORT.md 的数字直接从 summary json 读。
  summaryTrendStats: ["avg", "min", "med", "max", "p(90)", "p(95)", "p(99)"],
};

// setup 在所有 VU 启动前跑一次。不做登录（token 写不进 VU），只做环境探测。
export function setup() {
  const res = http.get(`${BASE_URL}/healthz`);
  // /healthz 未必存在，404/200 都算通；只要不是连接失败就放行。
  if (res.status >= 500 || res.error) {
    throw new Error(`gateway unreachable: ${res.status} ${res.error}`);
  }
}

// ---------- riderFlow -------------------------------------------------------

// 乘客流程：登录 → preview → start → 等 15s 看 driver_assigned。
// 每个 VU 的 session 在首次调用时建立并缓存到 VU-global（k6 里就是模块顶层的 let）。
let riderSession = null;

export function riderFlow() {
  if (!riderSession) {
    riderSession = login(pickUser(riders));
  }
  const headers = authHeaders(riderSession);

  // 1. preview。带 endpoint tag 分层看指标。
  const previewRes = http.post(
    `${BASE_URL}/trip/preview`,
    JSON.stringify(buildPreviewRequest(riderSession.userID)),
    { headers, tags: { endpoint: "preview" } }
  );
  const previewOK = check(previewRes, {
    "preview 2xx": (r) => r.status >= 200 && r.status < 300,
  });
  if (!previewOK) {
    // 思考时间内退出，别把失败的 VU 卡在这一轮。
    sleep(0.5);
    return;
  }

  // 从 preview 响应取 rideFareID。不同版本响应体略有差异，做两套兜底 path。
  const fareID =
    previewRes.json("data.rideFares.0.id") ||
    previewRes.json("data.rideFares.0.ID") ||
    "";

  sleep(0.3 + Math.random() * 0.5); // 用户思考 300~800ms

  // 2. start。限流 + 幂等锁都在 gateway 层，这里错误率是真实 SLO。
  const startRes = http.post(
    `${BASE_URL}/trip/start`,
    JSON.stringify(buildTripStartRequest(riderSession.userID, fareID)),
    { headers, tags: { endpoint: "start" } }
  );
  const startOK = check(startRes, {
    "start 2xx": (r) => r.status >= 200 && r.status < 300,
  });
  if (!startOK) {
    tripAssignedWithin15s.add(false);
    sleep(1);
    return;
  }

  // 3. WebSocket 连 /ws/riders，等 driver_assigned 事件，最多 15s。
  //    超时记为未分配。成功进 true 分支。
  const wsURL = BASE_URL.replace(/^http/, "ws") + "/ws/riders";
  let assigned = false;
  ws.connect(wsURL, { headers }, function (socket) {
    socket.setTimeout(() => socket.close(), 15000);
    socket.on("message", (msg) => {
      if (typeof msg === "string" && msg.indexOf("driver_assigned") >= 0) {
        assigned = true;
        socket.close();
      }
    });
    socket.on("close", () => {}); // 被 timeout 或主动关闭都走这
  });
  tripAssignedWithin15s.add(assigned);

  sleep(1);
}

// ---------- driverFlow ------------------------------------------------------

let driverSession = null;

export function driverFlow() {
  if (!driverSession) {
    driverSession = login(pickUser(drivers));
  }
  const headers = authHeaders(driverSession);

  // 司机只连 WebSocket 监听 trip_request，整个 scenario 持续 17min，
  // 每个 VU 的 ws 连接保持到脚本结束；ws.connect 是阻塞式的，回调里按事件处理。
  const wsURL = BASE_URL.replace(/^http/, "ws") + "/ws/drivers";
  ws.connect(wsURL, { headers }, function (socket) {
    // 10 分钟后主动关连接，避免 VU 累积挂连接数爆。
    // 17min 外的 VU 让 k6 graceful 自己收。
    socket.setTimeout(() => socket.close(), 10 * 60 * 1000);

    socket.on("message", (msg) => {
      // 收到 trip_request 就按 50% accept / 30% decline / 20% 忽略策略。
      if (typeof msg !== "string" || msg.indexOf("trip_request") < 0) {
        return;
      }
      const roll = Math.random();
      if (roll < 0.2) {
        return; // 超时分支：什么都不做
      }
      // accept / decline 都是发一条 ws message 给服务端（WebSocket 约定）。
      // 消息格式：{"type":"driver_response","action":"accept","tripID":"..."}
      // 真实 tripID 从 msg 里 parse；失败就丢弃。
      let tripID = "";
      try {
        const parsed = JSON.parse(msg);
        tripID = parsed.tripID || parsed.trip_id || (parsed.data && parsed.data.tripID) || "";
      } catch (_) {
        return;
      }
      if (!tripID) return;
      const action = roll < 0.7 ? "accept" : "decline";
      socket.send(JSON.stringify({ type: "driver_response", action, tripID }));
    });
  });
}

// ---------- attackerFlow ----------------------------------------------------

let attackerSession = null;

export function attackerFlow() {
  if (!attackerSession) {
    attackerSession = login(pickUser(attackers));
  }
  const headers = authHeaders(attackerSession);

  // 伪造一个格式合法（24 位 hex）但大概率不存在的 tripID，打 GET /trip/:id。
  // Bloom miss 预期返回 404，命中 = 被拦 = 指标 true。
  const fakeID = randomHexID(24);
  const res = http.get(`${BASE_URL}/trip/${fakeID}`, {
    headers,
    tags: { endpoint: "trip_get" },
  });
  attackerBlockedByBloom.add(res.status === 404);

  // 不 sleep：constant-arrival-rate 自己控速率，sleep 会拉低实际 rps。
}
