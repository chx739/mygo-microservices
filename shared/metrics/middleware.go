// middleware.go 提供 HTTP 中间件，记录每次请求的 duration 与 status。
// 对应 `演唱会压测实现方案.md` § 3.Step 1 middleware 职责。
//
// 核心决策：path 走「硬编码白名单 + 不匹配归 other」。
// Why：Prometheus Counter/Histogram 的 label value 基数直接决定内存占用。
// 如果直接把 r.URL.Path 当 label，攻击者传 `/trip/<任意字符串>` 就会引爆
// series 数量。白名单把路径折叠成固定集合（≈13 条），防止高基数。
package metrics

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// pathTemplate 把具体 URL 路径折叠为模板。
// 命中白名单前缀返回模板；否则返回 "other"。
//
// 白名单维护：新增业务 endpoint 时必须在此处同步添加，否则指标会被归到 other。
// 对照方案 § 3.Step 1 中的 path 列表。
func pathTemplate(p string) string {
	// 去查询串。
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	switch p {
	case "/auth/register", "/auth/login", "/auth/refresh", "/auth/logout", "/auth/me",
		"/trip/preview", "/trip/start",
		"/ws/drivers", "/ws/riders",
		"/webhook/stripe",
		"/metrics", "/healthz":
		return p
	}
	// /trip/:id 形式：Step 4.5 新增的透传接口。长度 > 6 且以 /trip/ 开头即归此模板。
	if strings.HasPrefix(p, "/trip/") && len(p) > len("/trip/") {
		return "/trip/:id"
	}
	return "other"
}

// statusRecorder 包装 ResponseWriter 捕获 status code。
// Why: stdlib ResponseWriter 不暴露 WriteHeader 调用记录；不引入 httpsnoop 之类
// 第三方依赖，手写一个最小 wrapper 更轻。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader 首次调用时记录 status。多次调用只记第一次（符合 HTTP 语义）。
func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write 在业务代码未显式 WriteHeader 时，首次 Write 会隐式走 200。
// 这里把 status 默认值设为 200，与 net/http 行为一致。
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Hijack 透传到底层 ResponseWriter，让 gorilla/websocket 等库能完成 HTTP→WS 升级。
// 不实现这个方法时，/ws/* 路由会失败：`websocket: response does not implement http.Hijacker`，
// Round 2 重跑就是这个 bug 让 trip_assigned_within_15s 永远 0%。
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics.statusRecorder: underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// Middleware 包装下游 handler，记录 HTTP 请求数与耗时。
//
// 入参:
//
//	next: 下游处理链。
//
// 返回: 装饰后的 handler；内部 start 时间 → latency → Observe。
//
// 注意: 本中间件不应用于 /metrics endpoint 本身（会造成自我观测噪声，
// 且 promhttp.Handler() 的 path 已经被归为白名单值）。实践中放在 mux 外层
// 统一包住，/metrics 被归到 "/metrics" 模板，不会爆炸，影响可忽略。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		path := pathTemplate(r.URL.Path)
		method := r.Method
		status := rec.status
		if status == 0 {
			// 下游既没调用 WriteHeader 也没 Write：空响应。按 200 记。
			status = http.StatusOK
		}

		HTTPRequestsTotal.WithLabelValues(method, path, strconv.Itoa(status)).Inc()
		HTTPRequestDurationSeconds.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	})
}
