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

// returnScript removes and re-pushes in one step, so an entry is never left
// in neither list, and concurrent reapers push it only once.
var returnScript = redis.NewScript(`
if redis.call('LREM', KEYS[1], 1, ARGV[1]) == 0 then
  return 0
end
redis.call('RPUSH', KEYS[2], ARGV[1])
return 1
`)

// ReturnIfInFlight moves one entry from InFlight back to the queue. It reports
// false when the entry was no longer in flight.
func ReturnIfInFlight(ctx context.Context, rdb *redis.Client, raw string) (bool, error) {
	moved, err := returnScript.Run(ctx, rdb, []string{InFlight, BuildQueue}, raw).Int()
	return moved == 1, err
}

func ListInFlight(ctx context.Context, rdb *redis.Client) ([]string, error) {
	return rdb.LRange(ctx, InFlight, 0, -1).Result()
}
