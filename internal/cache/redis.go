package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Books-QAQ/tickets/internal/util"
	"github.com/redis/go-redis/v9"
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
