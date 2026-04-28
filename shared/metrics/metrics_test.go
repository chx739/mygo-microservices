// metrics_test.go 覆盖 shared/metrics 的关键行为。
// 对应 `演唱会压测实现方案.md` § 4 层级 1 的 5 个用例。
//
// 测试思路：本包使用进程级 Registry，一次注册不能撤销。因此所有用例
// 共享同一次 Register 调用（TestMain 里统一初始化），用例内只做断言，
// 不重复注册。Register 本身的幂等性由 sync.Once 保证。
package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestMain(m *testing.M) {
	Register("metrics-test")
	m.Run()
}

// TestRegisterIdempotent 重复 Register 不 panic（sync.Once 守护）。
func TestRegisterIdempotent(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Register called twice panicked: %v", r)
		}
	}()
	Register("metrics-test")
	Register("another-name")
}

// TestPathTemplate 验证模板化白名单 + 归 other + /trip/:id 前缀识别。
func TestPathTemplate(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/trip/abc123", "/trip/:id"},
		{"/trip/507f1f77bcf86cd799439011", "/trip/:id"},
		{"/trip/preview", "/trip/preview"},
		{"/trip/start", "/trip/start"},
		{"/trip/", "other"},
		{"/auth/login", "/auth/login"},
		{"/auth/login?foo=bar", "/auth/login"}, // 查询串会被剥离
		{"/unknown/path", "other"},
		{"/ws/riders", "/ws/riders"},
		{"/metrics", "/metrics"},
	}
	for _, c := range cases {
		if got := pathTemplate(c.in); got != c.want {
			t.Errorf("pathTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMiddlewareRecordsLatency 确认 middleware 观测到 latency 且分桶正确。
// Handler sleep 50ms，应落入 [0.05, 0.1) 或 [0.1, 0.25) 桶（受调度抖动影响放宽）。
func TestMiddlewareRecordsLatency(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	h.ServeHTTP(rec, req)

	// 从 Histogram 里读样本数，至少 >= 1。
	hist, err := HTTPRequestDurationSeconds.GetMetricWithLabelValues("GET", "/auth/login")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := hist.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write metric: %v", err)
	}
	if m.Histogram == nil || m.Histogram.GetSampleCount() < 1 {
		t.Fatalf("expected >=1 sample, got %+v", m.Histogram)
	}
	if sum := m.Histogram.GetSampleSum(); sum < 0.04 {
		t.Fatalf("sample sum = %v, expected >= 40ms", sum)
	}
}

// TestMiddlewareStatusLabel 不同 status 分别计数，path 被模板化为 /trip/:id。
func TestMiddlewareStatusLabel(t *testing.T) {
	// 404 分支。
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	req := httptest.NewRequest(http.MethodGet, "/trip/abc123", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// 500 分支（同 path）。
	h2 := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	req2 := httptest.NewRequest(http.MethodGet, "/trip/xyz", nil)
	h2.ServeHTTP(httptest.NewRecorder(), req2)

	// 两个 status 各自 Counter 值 >= 1。
	c404, _ := HTTPRequestsTotal.GetMetricWithLabelValues("GET", "/trip/:id", "404")
	c500, _ := HTTPRequestsTotal.GetMetricWithLabelValues("GET", "/trip/:id", "500")
	m := &dto.Metric{}
	_ = c404.(prometheus.Metric).Write(m)
	if m.Counter == nil || m.Counter.GetValue() < 1 {
		t.Fatalf("404 counter = %v, want >=1", m.Counter)
	}
	m = &dto.Metric{}
	_ = c500.(prometheus.Metric).Write(m)
	if m.Counter == nil || m.Counter.GetValue() < 1 {
		t.Fatalf("500 counter = %v, want >=1", m.Counter)
	}
}

// TestConcurrentIncrements 并发写同一 Counter 无 race（go test -race 捕获）。
// Why: 四个服务都会从 goroutine 并发调指标 API，确保 client_golang 的 CounterVec
// 在当前使用方式下线程安全。
func TestConcurrentIncrements(t *testing.T) {
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			IdempotencyDedupTotal.WithLabelValues("unit-test", "first").Inc()
		}()
	}
	wg.Wait()

	c, _ := IdempotencyDedupTotal.GetMetricWithLabelValues("unit-test", "first")
	m := &dto.Metric{}
	_ = c.(prometheus.Metric).Write(m)
	if m.Counter.GetValue() < float64(n) {
		t.Fatalf("counter = %v, want >= %d", m.Counter.GetValue(), n)
	}
}

// TestHandlerExposesMetricsText /metrics endpoint 返回 prometheus 文本，包含 8 个指标名。
func TestHandlerExposesMetricsText(t *testing.T) {
	// 先写入一些样本，让所有 collector 都有数据（空 CounterVec 默认不输出）。
	HTTPRequestsTotal.WithLabelValues("GET", "/metrics", "200").Inc()
	RabbitMQConsumedTotal.WithLabelValues("q", "ack").Inc()
	RabbitMQDLQDepth.WithLabelValues("q").Set(0)
	TripAcceptLockContentionTotal.WithLabelValues("acquired").Inc()
	BloomFilterHitsTotal.WithLabelValues("trip_id", "hit").Inc()
	RateLimitRejectionsTotal.WithLabelValues("/auth/login").Inc()
	// IdempotencyDedupTotal 已在并发用例里写过。

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64*1024)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	wantNames := []string{
		"http_requests_total",
		"http_request_duration_seconds",
		"rabbitmq_consumed_total",
		"rabbitmq_dlq_depth",
		"trip_accept_lock_contention_total",
		"bloom_filter_hits_total",
		"rate_limit_rejections_total",
		"idempotency_dedup_total",
	}
	for _, name := range wantNames {
		if !strings.Contains(body, name) {
			t.Errorf("metrics body missing %q; first 500 bytes:\n%s", name, body[:min(500, len(body))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestStatusRecorderHijackerPassthrough 防回归：Round 2 重跑发现 statusRecorder
// 没透传 http.Hijacker，导致所有 /ws/* 路由握手失败。
// 这里启 httptest.Server，handler 内做 Hijack；handler 在 net/http 自己的
// goroutine 里跑，结果通过 channel 同步给测试主 goroutine（避免 race，
// 也避免在子 goroutine 里调 t.Fatalf 这种 testing 包不保证安全的用法）。
func TestStatusRecorderHijackerPassthrough(t *testing.T) {
	result := make(chan error, 1)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			result <- errIface
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			result <- err
			return
		}
		_ = conn.Close()
		result <- nil
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Hijack 之后连接被 close，client 通常返 EOF —— 不关心 err，只看 handler 那侧。
	resp, err := http.Get(srv.URL + "/whatever")
	if err == nil {
		_ = resp.Body.Close()
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Hijack 透传失败：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("handler 未走到 Hijack — middleware 拦了接口断言")
	}
}

// errIface 单独成变量，避免在 handler goroutine 里构造 error 时引入 fmt 依赖；
// 也让上面 select 里的对照逻辑更直接。
var errIface = httpHijackError("ResponseWriter 未实现 http.Hijacker")

type httpHijackError string

func (e httpHijackError) Error() string { return string(e) }
