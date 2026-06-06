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
//   - attacker_blocked_by_bloom (Rate)：attacker 没拿到 200 的比例（防穿透成功率）。
//     语义说明：原本只算 404=Bloom 命中，但高并发下 5xx/429/超时也是「攻击没成功」，
//     因此统计口径用 status !== 200。Round 2 重跑发现纯 404 口径在 19× 流量下漏报为 53%。

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

// K6_SCALE 在弱机/单节点 minikube 上等比例缩流量（保留 17min 时间线，
// 只动数值不动 scenario 结构）。Math.ceil 保证 SCALE 很小时也至少 1 VU。
const SCALE = Number(__ENV.K6_SCALE || 1);
const scaled = (n) => Math.max(1, Math.ceil(n * SCALE));

export const options = {
  scenarios: {
    riders: {
      executor: "ramping-vus",
      startVUs: scaled(50),
      stages: [
        { duration: "1m", target: scaled(50) },    // 基线
        { duration: "2m", target: scaled(2000) },  // 爬升
        { duration: "10m", target: scaled(2000) }, // 峰值（Chaos Workflow 在此窗口内注入）
        { duration: "2m", target: scaled(300) },   // 回落
        { duration: "2m", target: scaled(50) },    // 恢复
      ],
      exec: "riderFlow",
      gracefulRampDown: "10s",
    },
    drivers: {
      executor: "constant-vus",
      // 控制变量实验：driver 固定 100，不随 K6_SCALE 缩放。
      // 配合 K6_SCALE=0.1（rider 峰值 scaled(2000)=200），刻意做成 rider:driver=2:1
      // 的轻载局，用于坐实"Round2 全量 trip_assigned 崩塌=单机饱和而非逻辑错"。
      vus: 100,
      duration: "17m",
      exec: "driverFlow",
      startTime: "30s", // 等 rider 有订单再让 driver 就位，避免空跑
    },
    attackers: {
      executor: "constant-arrival-rate",
      rate: scaled(500),
      timeUnit: "1s",
      duration: "17m",
      preAllocatedVUs: scaled(100),
      maxVUs: scaled(200),
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

// 乘客流程：登录 → 先连 WS → 在 open 回调里 preview → start → 等 15s 看 driver_assigned。
// 每个 VU 的 session 在首次调用时建立并缓存到 VU-global（k6 里就是模块顶层的 let）。
//
// 为什么 WS 必须先连（subscribe-after-publish 竞态修复）：
// gateway 的 connManager.SendMessage 对未注册的 userID 直接 ErrConnectionNotFound
// 丢消息，无离线缓冲（shared/messaging/connection_messager.go:78-91）。原来的
// preview→start→connect 顺序下，SCALE 小时整条派单链路 <1s 跑完，driver_assigned
// 在 rider 的 WS 注册进 connManager 之前就被丢掉 → trip_assigned 永远≈0（实测 0.74%）。
// 真实手机 App 同样是开 App 即挂 WS、不是下单才连。故把 preview/start 移进
// socket.on("open")：握手成功时 gateway 已 connManager.Add + 起 driver_assigned
// 消费者（ws.go:33/43-49），此后再发 /trip/start 才不会漏接通知。
let riderSession = null;

export function riderFlow() {
  if (!riderSession) {
    riderSession = login(pickUser(riders));
  }
  const headers = authHeaders(riderSession);

  // 没有 userID 的旧 seed 记录：WS 路由依赖 userID，直接记 false 不试连（原语义）。
  if (!riderSession.userID) {
    tripAssignedWithin15s.add(false);
    sleep(1);
    return;
  }

  const wsURL =
    BASE_URL.replace(/^http/, "ws") +
    `/ws/riders?userID=${encodeURIComponent(riderSession.userID)}`;

  // 这三个标志必须在 ws.connect 外层作用域，回调闭包写、连接返回后结算。
  let opened = false; // WS 是否握手成功（false = WS 升级坏，必须暴露成失败，防 Hijacker 类回归被静默掩盖）
  let startedOK = false; // start 2xx，才计入 trip_assigned 分母
  let startFailed = false; // start 明确失败 → 记 false（对齐原语义）
  let assigned = false;

  ws.connect(wsURL, { headers }, function (socket) {
    socket.on("open", () => {
      opened = true;

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
        // 对齐原语义：preview 失败不计入 trip_assigned 分母。
        socket.close();
        return;
      }

      // 从 preview 响应取 rideFareID。不同版本响应体略有差异，做两套兜底 path。
      const fareID =
        previewRes.json("data.rideFares.0.id") ||
        previewRes.json("data.rideFares.0.ID") ||
        "";

      sleep(0.3 + Math.random() * 0.5); // 用户思考 300~800ms（此刻无下行消息，阻塞安全）

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
        startFailed = true;
        socket.close();
        return;
      }

      // 3. start 成功，开 15s 等待窗口看 driver_assigned。
      startedOK = true;
      socket.setTimeout(() => socket.close(), 15000);
    });

    socket.on("message", (msg) => {
      if (typeof msg === "string" && msg.indexOf("driver_assigned") >= 0) {
        assigned = true;
        socket.close();
      }
    });

    socket.on("close", () => {}); // timeout / 主动关 / preview|start 失败都走这
  });

  // ws.connect 阻塞返回后结算，严格保留原指标分支语义：
  //   WS 没连上   → 记 false（真失败，必须暴露）
  //   preview 失败 → 不记（不计入派单分母）
  //   start 失败   → 记 false
  //   start 成功   → 记 assigned
  if (!opened) {
    tripAssignedWithin15s.add(false);
  } else if (startFailed) {
    tripAssignedWithin15s.add(false);
  } else if (startedOK) {
    tripAssignedWithin15s.add(assigned);
  }

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
  // gateway ws.go:71-81 要求 userID + packageSlug 两个 query param 都非空。
  // packageSlug 必须和 rider 选的 fare 一致：driver-service 用 packageSlug
  // 作 GEO key 分区（services/driver-service/service.go:112-118），司机池
  // 按 slug 隔离。riderFlow 取 previewRes.data.rideFares[0]，trip-service
  // getBaseFares 顺序是 suv → sedan → van → luxury（service.go:248-260），
  // 所以这里写死 "suv" 跟 riders 对齐。Round 2 重跑时 driver 用 luxury
  // → "Found suitable drivers 0" → trip_assigned_within_15s 永 0%。
  if (!driverSession.userID) return;
  const wsURL =
    BASE_URL.replace(/^http/, "ws") +
    `/ws/drivers?userID=${encodeURIComponent(driverSession.userID)}&packageSlug=suv`;
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
      // 消息格式（gateway 的 QueueConsumer + TripEventData 决定）：
      //   {"type":"driver.cmd.trip_request","data":{"trip":{"id":"...","userID":"...",...}}}
      // 所以 tripID 在 data.trip.id；之前写死 data.tripID 取不到，driver 永远不回 accept。
      let tripID = "";
      try {
        const parsed = JSON.parse(msg);
        tripID =
          (parsed.data && parsed.data.trip && parsed.data.trip.id) ||
          parsed.tripID ||
          parsed.trip_id ||
          "";
      } catch (_) {
        return;
      }
      if (!tripID) return;
      // gateway ws.go:160 用 msgType 决定 RoutingKey；
      // type 必须是 contracts.DriverCmdTripAccept / DriverCmdTripDecline
      // ("driver.cmd.trip_accept" / "driver.cmd.trip_decline")。
      // 之前用 "driver_response" 不在 switch 里，gateway 直接打 unknown 并丢。
      const action = roll < 0.7 ? "accept" : "decline";
      const type = action === "accept" ? "driver.cmd.trip_accept" : "driver.cmd.trip_decline";
      socket.send(JSON.stringify({ type, data: { tripID } }));
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
  // 任何非 200 都算「攻击未成功」：Bloom 命中→404、限流→429、上游错→5xx 都是防御生效。
  // 只有 200 才意味着 Bloom 漏 + Mongo 真有这个 ID（数学上接近 0）。
  const fakeID = randomHexID(24);
  const res = http.get(`${BASE_URL}/trip/${fakeID}`, {
    headers,
    tags: { endpoint: "trip_get" },
  });
  attackerBlockedByBloom.add(res.status !== 200);

  // 不 sleep：constant-arrival-rate 自己控速率，sleep 会拉低实际 rps。
}
