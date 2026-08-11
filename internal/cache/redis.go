package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
)

const (
	PurchaseQueueKey = "queue:purchase"
)

func NewRedisClient(config util.Config) (*redis.Client, error) {
	addr := fmt.Sprintf("%s:%s", config.REDISHOST, config.REDISPORT)

	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     config.REDISPASSWORD,
		DB:           config.REDISDB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("cannot ping redis: %w", err)
	}

	return client, nil
}

func GetJSON[T any](ctx context.Context, client *redis.Client, key string) (T, bool, error) {
	var value T

	if client == nil {
		return value, false, nil
	}

	payload, err := client.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return value, false, nil
		}
		return value, false, err
	}

	if err := json.Unmarshal(payload, &value); err != nil {
		return value, false, err
	}

	return value, true, nil
}

func SetJSON(ctx context.Context, client *redis.Client, key string, value any, ttl time.Duration) error {
	if client == nil {
		return nil
	}

	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return client.Set(ctx, key, payload, ttl).Err()
}

func DeleteKey(ctx context.Context, client *redis.Client, key string) error {
	if client == nil {
		return nil
	}

	return client.Del(ctx, key).Err()
}

func SeatHoldKey(busID, seatID int32) string {
	return fmt.Sprintf("seat:hold:%d:%d", busID, seatID)
}

func PurchaseTaskKey(requestID string) string {
	return fmt.Sprintf("purchase:task:%s", requestID)
}

func RoutesQueryCacheKey(originTerminalID, destinationTerminalID int32, departureDate time.Time) string {
	return fmt.Sprintf(
		"routes:%d:%d:%s",
		originTerminalID,
		destinationTerminalID,
		departureDate.Format("2006-01-02"),
	)
}

var releaseSeatHoldScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func AcquireSeatHold(ctx context.Context, client *redis.Client, busID, seatID int32, owner string, ttl time.Duration) (bool, error) {
	if client == nil {
		return true, nil
	}

	ok, err := client.SetNX(ctx, SeatHoldKey(busID, seatID), owner, ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

func ReleaseSeatHold(ctx context.Context, client *redis.Client, busID, seatID int32, owner string) error {
	if client == nil {
		return nil
	}

	return releaseSeatHoldScript.Run(ctx, client, []string{SeatHoldKey(busID, seatID)}, owner).Err()
}

func GetSeatHoldOwner(ctx context.Context, client *redis.Client, busID, seatID int32) (string, error) {
	if client == nil {
		return "", nil
	}

	value, err := client.Get(ctx, SeatHoldKey(busID, seatID)).Result()
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}

	return value, nil
}

func EnqueueJSON(ctx context.Context, client *redis.Client, queueKey string, value any) error {
	if client == nil {
		return nil
	}

	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return client.RPush(ctx, queueKey, payload).Err()
}

func DequeueJSONBlocking(ctx context.Context, client *redis.Client, queueKey string, timeout time.Duration, target any) (bool, error) {
	if client == nil {
		return false, nil
	}

	values, err := client.BLPop(ctx, timeout, queueKey).Result()
	if err != nil {
		if err == redis.Nil {
			return false, nil
		}
		return false, err
	}

	if len(values) < 2 {
		return false, nil
	}

	if err := json.Unmarshal([]byte(values[1]), target); err != nil {
		return false, err
	}

	return true, nil
}

func AllowFixedWindow(ctx context.Context, client *redis.Client, key string, limit int64, window time.Duration) (bool, time.Duration, error) {
	if client == nil || limit <= 0 || window <= 0 {
		return true, 0, nil
	}

	count, err := client.Incr(ctx, key).Result()
	if err != nil {
		return true, 0, err
	}

	if count == 1 {
		if err := client.Expire(ctx, key, window).Err(); err != nil {
			return true, 0, err
		}
	}

	ttl, err := client.TTL(ctx, key).Result()
	if err != nil {
		return true, 0, err
	}
	if ttl < 0 {
		ttl = window
	}

	return count <= limit, ttl, nil
}

var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local refill_rate = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])

local data = redis.call("HMGET", key, "tokens", "ts")
local tokens = tonumber(data[1])
local ts = tonumber(data[2])

if tokens == nil then
  tokens = capacity
end

if ts == nil then
  ts = now
end

local delta = now - ts
if delta < 0 then
  delta = 0
end

local replenished = tokens + (delta / 1000.0) * refill_rate
if replenished > capacity then
  replenished = capacity
end

local allowed = 0
local remaining = replenished
local retry_after_ms = 0

if replenished >= requested then
  allowed = 1
  remaining = replenished - requested
else
  retry_after_ms = math.ceil(((requested - replenished) / refill_rate) * 1000)
  if retry_after_ms < 0 then
    retry_after_ms = 0
  end
end

redis.call("HMSET", key, "tokens", remaining, "ts", now)
redis.call("PEXPIRE", key, ttl)

return {allowed, remaining, retry_after_ms}
`)

func AllowTokenBucket(ctx context.Context, client *redis.Client, key string, capacity int64, refillRate float64, requested int64) (bool, time.Duration, error) {
	if client == nil || capacity <= 0 || refillRate <= 0 || requested <= 0 {
		return true, 0, nil
	}

	nowMs := time.Now().UnixMilli()
	ttlMs := int64(math.Ceil((float64(capacity) / refillRate) * 2000.0))
	if ttlMs < 1000 {
		ttlMs = 1000
	}

	values, err := tokenBucketScript.Run(ctx, client, []string{key}, nowMs, capacity, refillRate, requested, ttlMs).Result()
	if err != nil {
		return true, 0, err
	}

	result, ok := values.([]interface{})
	if !ok || len(result) != 3 {
		return true, 0, fmt.Errorf("unexpected token bucket result")
	}

	allowed, ok := result[0].(int64)
	if !ok {
		return true, 0, fmt.Errorf("unexpected token bucket allow flag")
	}

	retryAfterMs, ok := result[2].(int64)
	if !ok {
		return true, 0, fmt.Errorf("unexpected token bucket retry value")
	}

	return allowed == 1, time.Duration(retryAfterMs) * time.Millisecond, nil
}
