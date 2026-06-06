package cache

import (
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

// LocalCache 是基于 ristretto v2 的进程内 L1 缓存封装。
//
// 设计点：
//   - **nil receiver 安全**：所有方法对 nil *LocalCache 都做空操作（Get 返 false、
//     Set/Del 静默、Wait 立即返回）。这样上层不传 L1 即等价于"无 L1"的旧行为，
//     调用点无需 nil 判断。
//   - 值类型固定为 []byte（业务侧 JSON 序列化），简化 ristretto 泛型实例化。
//   - cost 用 len(value) 做近似（字节数），配合 MaxCost 上限。
//
// 为什么选 ristretto：TinyLFU 准入 + 采样 LRU，是 Caffeine 在 Go 圈的对标实现，
// 比朴素 LRU 在高基数读路径上抗扫描污染更好。代价：Set 走异步 buffer——测试
// 中需要 Wait() 强同步保证 Set 后立即 Get 命中；生产代码不强 Wait（best-effort
// 缓存语义可接受）。
type LocalCache struct {
	c *ristretto.Cache[string, []byte]
}

// NewLocalCache 构造一个新的本地缓存。
//   - numCounters: TinyLFU 频次计数器数量，推荐为 MaxItem × 10。
//   - maxCostBytes: 整缓存最大成本（这里语义=字节）。
func NewLocalCache(numCounters int64, maxCostBytes int64) (*LocalCache, error) {
	c, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: numCounters,
		MaxCost:     maxCostBytes,
		BufferItems: 64,
	})
	if err != nil {
		return nil, err
	}
	return &LocalCache{c: c}, nil
}

// Get 返回 key 对应的字节切片副本。命中返回 (val, true)，未命中 (nil, false)。
// nil receiver 等同未命中。
func (l *LocalCache) Get(key string) ([]byte, bool) {
	if l == nil || l.c == nil {
		return nil, false
	}
	v, ok := l.c.Get(key)
	if !ok {
		return nil, false
	}
	return v, true
}

// Set 写入 key→value，附带 TTL。
// 返回 false 表示被 TinyLFU 准入拒绝（罕见，仅 best-effort）。
// nil receiver 等同 no-op。
func (l *LocalCache) Set(key string, value []byte, ttl time.Duration) bool {
	if l == nil || l.c == nil {
		return false
	}
	cost := int64(len(value))
	if cost == 0 {
		cost = 1
	}
	return l.c.SetWithTTL(key, value, cost, ttl)
}

// Del 删除 key。nil receiver 等同 no-op。
func (l *LocalCache) Del(key string) {
	if l == nil || l.c == nil {
		return
	}
	l.c.Del(key)
}

// Wait 阻塞直到所有 pending 的 Set 都已应用到底层 map。
// 仅用于测试确定性；生产代码不需要也不应调用（会影响 throughput）。
func (l *LocalCache) Wait() {
	if l == nil || l.c == nil {
		return
	}
	l.c.Wait()
}

// Close 释放内部资源（关闭 metrics 等）。
func (l *LocalCache) Close() {
	if l == nil || l.c == nil {
		return
	}
	l.c.Close()
}
