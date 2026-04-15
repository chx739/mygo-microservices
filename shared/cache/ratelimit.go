package cache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// tripRateLimitKeyPrefix 是下单滑动窗口限流 key 的统一前缀。
	// 完整 key 形如：ratelimit:trip:{userID}
	tripRateLimitKeyPrefix = "ratelimit:trip:"
)

var (
	// slidingWindowScript 通过 Lua 保证“删过期 -> 计数 -> 写入”三个步骤原子执行。
	// 这样在高并发下也不会出现计数穿透。
	slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]

-- 1) 先删除窗口外的旧请求记录
redis.call('ZREMRANGEBYSCORE', key, 0, now - window)

-- 2) 再统计窗口内请求数量
local count = redis.call('ZCARD', key)

if count < limit then
    -- 3) 数量未达阈值，允许本次请求并写入当前请求记录
    redis.call('ZADD', key, now, member)
    redis.call('PEXPIRE', key, window)
    return 1
end

-- 超过阈值，拒绝本次请求
return 0
`)

	// slidingWindowSeq 用于生成 member 去重后缀，避免同毫秒内 member 冲突。
	slidingWindowSeq uint64
)

// SlidingWindowAllow 使用 Redis 有序集合 + Lua 实现滑动窗口限流。
// 参数说明：
// - userID: 限流主体（当前按用户维度限流）。
// - limit: 窗口内允许的最大请求数。
// - window: 滑动窗口大小。
// 返回值：
// - true: 允许通过。
// - false: 命中限流，应该拒绝。
func SlidingWindowAllow(
	ctx context.Context,
	rdb redis.UniversalClient,
	userID string,
	limit int,
	window time.Duration,
) (bool, error) {
	if rdb == nil {
		return false, fmt.Errorf("redis client is required for sliding window rate limit")
	}

	trimmedUserID := strings.TrimSpace(userID)
	if trimmedUserID == "" {
		return false, fmt.Errorf("userID is required for sliding window rate limit")
	}

	if limit <= 0 {
		return false, fmt.Errorf("limit must be greater than zero")
	}

	if window <= 0 {
		return false, fmt.Errorf("window must be greater than zero")
	}

	nowMillis := time.Now().UnixMilli()
	member := buildSlidingWindowMember(nowMillis)

	result, err := slidingWindowScript.Run(
		ctx,
		rdb,
		[]string{tripRateLimitKeyPrefix + trimmedUserID},
		nowMillis,
		window.Milliseconds(),
		limit,
		member,
	).Int()
	if err != nil {
		return false, err
	}

	return result == 1, nil
}

func buildSlidingWindowMember(nowMillis int64) string {
	seq := atomic.AddUint64(&slidingWindowSeq, 1)
	return strconv.FormatInt(nowMillis, 10) + "-" + strconv.FormatUint(seq, 10)
}
