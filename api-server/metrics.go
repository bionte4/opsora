package main

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"devops-allinone/shared/queue"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

var (
	webhooksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opsora_webhooks_total",
		Help: "GitHub webhook deliveries by event and outcome.",
	}, []string{"event", "result"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opsora_http_request_duration_seconds",
		Help:    "API request latency by route, method, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method", "code"})
)

// registerQueueGauges reports queue lengths at scrape time. With several API
// replicas each reports the same value, so dashboards use max().
func registerQueueGauges(rdb *redis.Client) {
	for _, key := range []string{queue.BuildQueue, queue.InFlight} {
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "opsora_queue_length",
			Help:        "Entries in a Redis build list.",
			ConstLabels: prometheus.Labels{"queue": key},
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			n, err := rdb.LLen(ctx, key).Result()
			if err != nil {
				return math.NaN()
			}
			return float64(n)
		})
	}
}

// metricsMiddleware labels requests by route pattern, not raw path, so build
// IDs do not create a new series per request.
func metricsMiddleware(c *fiber.Ctx) error {
	start := time.Now()
	err := c.Next()
	route := c.Route().Path
	if route == "" || route == "/" && c.Path() != "/" {
		route = "unmatched"
	}
	code := c.Response().StatusCode()
	if err != nil {
		code = fiber.StatusInternalServerError
		var fe *fiber.Error
		if errors.As(err, &fe) {
			code = fe.Code
		}
	}
	requestDuration.WithLabelValues(route, c.Method(), strconv.Itoa(code)).Observe(time.Since(start).Seconds())
	return err
}
