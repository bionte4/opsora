package queue

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	BuildQueue = "build_queue"
	// InFlight holds build IDs a worker has taken but not acknowledged, so a
	// crashed worker cannot lose them.
	InFlight = "build_processing"
)

func Enqueue(ctx context.Context, rdb *redis.Client, id int64) error {
	return rdb.RPush(ctx, BuildQueue, strconv.FormatInt(id, 10)).Err()
}

// Dequeue blocks until a build ID is available and moves it to InFlight.
// It returns redis.Nil when timeout passes with an empty queue.
func Dequeue(ctx context.Context, rdb *redis.Client, timeout time.Duration) (string, error) {
	return rdb.BLMove(ctx, BuildQueue, InFlight, "LEFT", "RIGHT", timeout).Result()
}

// Ack removes one in-flight entry for the build.
func Ack(ctx context.Context, rdb *redis.Client, raw string) error {
	return rdb.LRem(ctx, InFlight, 1, raw).Err()
}

func Forget(ctx context.Context, rdb *redis.Client, id int64) error {
	return rdb.LRem(ctx, InFlight, 0, strconv.FormatInt(id, 10)).Err()
}

func Requeue(ctx context.Context, rdb *redis.Client, id int64) error {
	raw := strconv.FormatInt(id, 10)
	_, err := rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.LRem(ctx, InFlight, 0, raw)
		pipe.RPush(ctx, BuildQueue, raw)
		return nil
	})
	return err
}

// ReturnIfInFlight puts an entry back on the queue only if this caller is the
// one that removed it from InFlight, so concurrent reapers push it once.
func ReturnIfInFlight(ctx context.Context, rdb *redis.Client, raw string) (bool, error) {
	removed, err := rdb.LRem(ctx, InFlight, 1, raw).Result()
	if err != nil || removed == 0 {
		return false, err
	}
	return true, rdb.RPush(ctx, BuildQueue, raw).Err()
}

func ListInFlight(ctx context.Context, rdb *redis.Client) ([]string, error) {
	return rdb.LRange(ctx, InFlight, 0, -1).Result()
}
