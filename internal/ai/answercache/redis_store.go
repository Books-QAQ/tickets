package answercache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore Store 的 Redis 实现（生产用）。
//
// 存储布局（都在 `cs:` 前缀下，与业务缓存隔离）：
//
//	cs:ac:obj:{sha1(规范问题)}:{category}   → 单条 Entry（精确命中路径）
//	cs:ac:idx:{category}                    → 该分类的 Entry 数组（语义匹配路径）
//	cs:ac:pend:{sha1(规范问题)}:{category}  → 出现次数（准入判据）
type RedisStore struct{ Client *redis.Client }

func (r RedisStore) Get(ctx context.Context, key string) (string, bool, error) {
	if r.Client == nil {
		return "", false, nil
	}
	v, err := r.Client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (r RedisStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if r.Client == nil {
		return nil
	}
	return r.Client.Set(ctx, key, value, ttl).Err()
}

// Del 删除键（淘汰精确键时用）
func (r RedisStore) Del(ctx context.Context, keys ...string) error {
	if r.Client == nil || len(keys) == 0 {
		return nil
	}
	return r.Client.Del(ctx, keys...).Err()
}

// Incr 计数并（首次出现时）设置 TTL。Redis 的 INCR 本身是原子的；
// EXPIRE 只在 n==1 时设置，避免每次命中都延长候选池寿命。
func (r RedisStore) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if r.Client == nil {
		return 0, nil
	}
	pipe := r.Client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	if ttl > 0 {
		// NX 语义：仅在无 TTL 时设置，保证滑动过期不被反复重置
		pipe.ExpireNX(ctx, key, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}
