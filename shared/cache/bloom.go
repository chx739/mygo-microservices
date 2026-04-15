package cache

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const (
	// bloomSize 表示布隆过滤器位图大小。
	// 10_000_000 bit 约等于 1.25MB，内存开销可控，误判率也能保持在较低水平。
	bloomSize int64 = 10_000_000
)

// BloomAdd 把元素写入布隆过滤器（两个 hash 映射到两个 bit 位）。
// 注意：布隆过滤器只支持“可能存在/一定不存在”，不支持删除单个元素。
func BloomAdd(ctx context.Context, rdb redis.UniversalClient, filterKey, item string) error {
	if rdb == nil {
		return fmt.Errorf("redis client is required for bloom add")
	}

	trimmedFilterKey := strings.TrimSpace(filterKey)
	if trimmedFilterKey == "" {
		return fmt.Errorf("filterKey is required for bloom add")
	}

	trimmedItem := strings.TrimSpace(item)
	if trimmedItem == "" {
		return fmt.Errorf("item is required for bloom add")
	}

	h1 := bloomHash1(trimmedItem)
	h2 := bloomHash2(trimmedItem)

	pipe := rdb.Pipeline()
	pipe.SetBit(ctx, trimmedFilterKey, h1, 1)
	pipe.SetBit(ctx, trimmedFilterKey, h2, 1)

	_, err := pipe.Exec(ctx)
	return err
}

// BloomExists 判断元素在布隆过滤器中是否“可能存在”。
// 返回语义：
// - false: 一定不存在（至少一个 bit 为 0）。
// - true: 可能存在（两个 bit 都为 1，存在误判概率）。
func BloomExists(ctx context.Context, rdb redis.UniversalClient, filterKey, item string) (bool, error) {
	if rdb == nil {
		return false, fmt.Errorf("redis client is required for bloom exists")
	}

	trimmedFilterKey := strings.TrimSpace(filterKey)
	if trimmedFilterKey == "" {
		return false, fmt.Errorf("filterKey is required for bloom exists")
	}

	trimmedItem := strings.TrimSpace(item)
	if trimmedItem == "" {
		return false, fmt.Errorf("item is required for bloom exists")
	}

	h1 := bloomHash1(trimmedItem)
	h2 := bloomHash2(trimmedItem)

	pipe := rdb.Pipeline()
	bit1 := pipe.GetBit(ctx, trimmedFilterKey, h1)
	bit2 := pipe.GetBit(ctx, trimmedFilterKey, h2)

	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}

	return bit1.Val() == 1 && bit2.Val() == 1, nil
}

func bloomHash1(item string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(item))
	return int64(h.Sum32()) % bloomSize
}

func bloomHash2(item string) int64 {
	h := fnv.New32()
	_, _ = h.Write([]byte(item))

	// 使用不同 hash 函数并混入固定扰动，降低与 hash1 的相关性。
	const xorSeed uint32 = 0x9e3779b9
	return int64(h.Sum32()^xorSeed) % bloomSize
}
