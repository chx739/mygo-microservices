// 演唱会压测方案 § 3.Step 5：请求体构造器。
//
// 职责：把 /trip/preview /trip/start 的 body 构造和场馆坐标分布收敛到一处，
// concert.js 只负责调用，不关心坐标怎么抖、userID 从哪来。
//
// 场馆：广州天河体育中心 (23.1291°N, 113.2644°E)。
// 散场流量模型：绝大部分订单起点在场馆周围 3km 内，终点分散到 1~10km。
// 用均匀分布近似，真实场景是「起点集中 + 终点长尾」，但本次压测不追求
// 真实路网分布，只要 OSRM 收到的坐标合法、路径距离有变化即可。

const VENUE_LAT = 23.1291;
const VENUE_LON = 113.2644;

// randInRange 返回 [lo, hi) 的均匀分布浮点数。
function randInRange(lo, hi) {
  return lo + Math.random() * (hi - lo);
}

// randomConcertVenueCoord 生成场馆附近的随机坐标。
// radiusKm 控制散布半径：rider 起点用 3km，终点用 1~10km（在 buildTripRequest 里组合）。
// 近似算法：1° 纬度 ≈ 111km；经度按 cos(lat) 压缩。
export function randomConcertVenueCoord(radiusKm = 3) {
  const dLat = (randInRange(-radiusKm, radiusKm)) / 111;
  const dLon = (randInRange(-radiusKm, radiusKm)) / (111 * Math.cos((VENUE_LAT * Math.PI) / 180));
  return {
    latitude: VENUE_LAT + dLat,
    longitude: VENUE_LON + dLon,
  };
}

// buildPreviewRequest 构造 POST /trip/preview 的 JSON body。
// userID 允许为空字符串：未登录的 preview 也应该能走通（api-gateway 侧
// /trip/preview 目前不挂 authRequired，但 body 里 userID 必填）。
export function buildPreviewRequest(userID) {
  return {
    userID: userID,
    startLocation: randomConcertVenueCoord(2),     // 起点：场馆 2km 内
    endLocation: randomConcertVenueCoord(8),       // 终点：场馆 8km 范围
  };
}

// buildTripStartRequest 构造 POST /trip/start 的 JSON body。
// rideFareID 从 preview 响应里取，userID 取自登录 session。
export function buildTripStartRequest(userID, rideFareID) {
  return {
    userID: userID,
    rideFareID: rideFareID,
  };
}
