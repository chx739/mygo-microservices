// Package metrics 提供统一的 Prometheus 指标定义与 HTTP /metrics endpoint。
// 对应 `演唱会压测实现方案.md` § 3.Step 1。
//
// 本包导出 8 个指标：4 个 HTTP 相关 + 4 个业务相关。所有指标在进程启动时
// 通过 Register 一次性注册进全局 Registry；4 个服务共享本包，指标名、label
// key 统一，保证 Grafana 面板查询可以用相同表达式跨服务聚合。
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registerOnce 保证指标只注册一次。
// Why: 单测会多次调用 Register（不同用例共享同一个进程级 Registry），重复
// 注册同名 Collector 会让 prometheus 返回 AlreadyRegisteredError 并 panic。
var registerOnce sync.Once

// 全局指标变量（包级），注册后由 middleware / 业务代码通过这些变量写入。
// Why：不用每次 GetCounter 再查 registry，避免热路径上的 map 查找。
var (
	// HTTPRequestsTotal 统计每个 endpoint 的请求数，按 service/method/path/status 分维度。
	// 用途：QPS、错误率（status=~"5..")、成功率。
	// path 必须是模板化后的值（如 /trip/:id），不能是具体 ID，避免高基数爆内存。
	HTTPRequestsTotal *prometheus.CounterVec

	// HTTPRequestDurationSeconds 是 HTTP 请求耗时直方图。
	// buckets 按方案 § 3.Step 1 固定：5ms~5s，覆盖从 Redis 命中到 bcrypt 登录的延迟谱。
	HTTPRequestDurationSeconds *prometheus.HistogramVec

	// RabbitMQConsumedTotal 记录每个 consumer 的消费结果（ack/nack/requeue/dlq）。
	RabbitMQConsumedTotal *prometheus.CounterVec

	// RabbitMQDLQDepth 是各 DLQ 当前深度，由外部定时采集（不是实时 metric）。
	RabbitMQDLQDepth *prometheus.GaugeVec

	// TripAcceptLockContentionTotal 记录接单分布式锁的冲突次数。
	// label result: acquired / contended。
	TripAcceptLockContentionTotal *prometheus.CounterVec

	// BloomFilterHitsTotal 记录 Bloom 过滤器查询结果。
	// label filter: trip_id 等；label result: hit / miss_rejected / miss_fallthrough。
	BloomFilterHitsTotal *prometheus.CounterVec

	// RateLimitRejectionsTotal 记录被限流中间件拒绝的请求。
	RateLimitRejectionsTotal *prometheus.CounterVec

	// IdempotencyDedupTotal 记录消息/请求幂等去重命中。
	// label result: first / duplicate。
	IdempotencyDedupTotal *prometheus.CounterVec
)

// defaultBuckets 是 HTTP 延迟直方图的桶边界（秒）。
// 方案 § 3.Step 1 明示值，修改请同步更新方案文档。
var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Register 把全部指标注册进 prometheus 默认 Registry，并把 service 名写进
// ConstLabel。调用者应在进程启动时调用一次。重复调用幂等（sync.Once 守护）。
//
// 入参:
//
//	serviceName: 服务标识，写入所有指标的 service label，Grafana 面板按此过滤。
//
// 错误语义: 不返回 error；注册失败直接 panic（启动期失败优于运行期悄无声息）。
func Register(serviceName string) {
	registerOnce.Do(func() {
		constLabels := prometheus.Labels{"service": serviceName}

		HTTPRequestsTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "http_requests_total",
				Help:        "Total HTTP requests by method/path/status.",
				ConstLabels: constLabels,
			},
			[]string{"method", "path", "status"},
		)
		HTTPRequestDurationSeconds = prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:        "http_request_duration_seconds",
				Help:        "HTTP request duration in seconds.",
				Buckets:     defaultBuckets,
				ConstLabels: constLabels,
			},
			[]string{"method", "path"},
		)
		RabbitMQConsumedTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "rabbitmq_consumed_total",
				Help:        "RabbitMQ messages consumed, by queue and processing result.",
				ConstLabels: constLabels,
			},
			[]string{"queue", "result"},
		)
		RabbitMQDLQDepth = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:        "rabbitmq_dlq_depth",
				Help:        "Current depth of dead-letter queues.",
				ConstLabels: constLabels,
			},
			[]string{"queue"},
		)
		TripAcceptLockContentionTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "trip_accept_lock_contention_total",
				Help:        "Distributed lock outcomes for trip accept flow.",
				ConstLabels: constLabels,
			},
			[]string{"result"},
		)
		BloomFilterHitsTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "bloom_filter_hits_total",
				Help:        "Bloom filter query outcomes.",
				ConstLabels: constLabels,
			},
			[]string{"filter", "result"},
		)
		RateLimitRejectionsTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "rate_limit_rejections_total",
				Help:        "Requests rejected by rate limiter.",
				ConstLabels: constLabels,
			},
			[]string{"endpoint"},
		)
		IdempotencyDedupTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "idempotency_dedup_total",
				Help:        "Idempotency dedup outcomes for consumers / webhooks.",
				ConstLabels: constLabels,
			},
			[]string{"consumer", "result"},
		)

		prometheus.MustRegister(
			HTTPRequestsTotal,
			HTTPRequestDurationSeconds,
			RabbitMQConsumedTotal,
			RabbitMQDLQDepth,
			TripAcceptLockContentionTotal,
			BloomFilterHitsTotal,
			RateLimitRejectionsTotal,
			IdempotencyDedupTotal,
		)
	})
}

// Handler 返回 prometheus 抓取用的 /metrics http.Handler。
// 调用方在 mux 上 `mux.Handle("/metrics", metrics.Handler())`。
func Handler() http.Handler {
	return promhttp.Handler()
}
