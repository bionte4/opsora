package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"

	"devops-allinone/shared/queue"
	"devops-allinone/shared/store"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const chatopsLogLines = 20

var chatopsCommands = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "opsora_chatops_commands_total",
	Help: "ChatOps API calls by command and result.",
}, []string{"command", "result"})

func init() {
	for _, command := range []string{"auth", "status", "logs", "rerun", "rollback"} {
		for _, result := range []string{"ok", "denied", "unauthorized", "error"} {
			chatopsCommands.WithLabelValues(command, result)
		}
	}
}

func allowedChatSet(raw string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		if id := strings.TrimSpace(part); id != "" {
			set[id] = struct{}{}
		}
	}
	return set
}

// authorizeChat checks the shared token and the chat allowlist. Commands from
// any other chat are rejected here, so the notifier can forward every update.
func (a *app) authorizeChat(c *fiber.Ctx) error {
	if a.chatopsToken == "" {
		return jsonError(c, fiber.StatusServiceUnavailable, "chatops is disabled")
	}
	got := []byte(c.Get("Authorization"))
	want := []byte("Bearer " + a.chatopsToken)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		chatopsCommands.WithLabelValues("auth", "unauthorized").Inc()
		return jsonError(c, fiber.StatusUnauthorized, "invalid token")
	}
	chat := c.Get("X-Opsora-Chat-Id")
	if _, ok := a.allowedChats[chat]; !ok {
		chatopsCommands.WithLabelValues("auth", "denied").Inc()
		return jsonError(c, fiber.StatusForbidden, "chat is not allowed")
	}
	c.Locals("chatID", chat)
	return c.Next()
}

func (a *app) chatopsStatus(c *fiber.Ctx) error {
	limit := c.QueryInt("limit", 5)
	if limit < 1 || limit > 20 {
		return jsonError(c, fiber.StatusBadRequest, "limit must be between 1 and 20")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	builds, err := a.store.ListBuilds(ctx, limit)
	if err != nil {
		chatopsCommands.WithLabelValues("status", "error").Inc()
		log.Printf("chatops status: %v", err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to list builds")
	}
	chatopsCommands.WithLabelValues("status", "ok").Inc()
	return c.JSON(fiber.Map{"builds": builds})
}

func (a *app) chatopsBuild(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	build, err := a.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "build not found")
	}
	if err != nil {
		chatopsCommands.WithLabelValues("status", "error").Inc()
		return jsonError(c, fiber.StatusInternalServerError, "failed to load build")
	}
	chatopsCommands.WithLabelValues("status", "ok").Inc()
	return c.JSON(build)
}

func (a *app) chatopsLogs(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := a.store.GetBuild(ctx, id); errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "build not found")
	} else if err != nil {
		chatopsCommands.WithLabelValues("logs", "error").Inc()
		return jsonError(c, fiber.StatusInternalServerError, "failed to load build")
	}
	lines, err := a.store.TailBuildLogs(ctx, id, chatopsLogLines)
	if err != nil {
		chatopsCommands.WithLabelValues("logs", "error").Inc()
		log.Printf("chatops logs %d: %v", id, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to load logs")
	}
	chatopsCommands.WithLabelValues("logs", "ok").Inc()
	return c.JSON(fiber.Map{"build_id": id, "lines": lines})
}

func (a *app) chatopsRerun(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid build id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	source, err := a.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "build not found")
	}
	if err != nil {
		chatopsCommands.WithLabelValues("rerun", "error").Inc()
		return jsonError(c, fiber.StatusInternalServerError, "failed to load build")
	}
	if source.CommitSHA == "" {
		return jsonError(c, fiber.StatusUnprocessableEntity, "build has no commit to rerun")
	}

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return jsonError(c, fiber.StatusInternalServerError, "failed to create build")
	}
	payload, _ := json.Marshal(map[string]any{
		"source_build_id": source.ID,
		"chat_id":         c.Locals("chatID"),
	})
	build, created, err := a.store.CreateBuild(ctx, store.NewBuild{
		DeliveryID: "rerun-" + hex.EncodeToString(nonce[:]),
		Event:      "rerun",
		Payload:    payload,
		Repository: source.Repository,
		Ref:        source.Ref,
		CommitSHA:  source.CommitSHA,
		Actor:      "chat:" + c.Locals("chatID").(string),
	})
	if err != nil || !created {
		chatopsCommands.WithLabelValues("rerun", "error").Inc()
		log.Printf("chatops rerun %d: created=%v err=%v", id, created, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to create build")
	}
	if err := queue.Enqueue(ctx, a.redis, build.ID); err != nil {
		chatopsCommands.WithLabelValues("rerun", "error").Inc()
		log.Printf("chatops enqueue %d: %v", build.ID, err)
		return jsonError(c, fiber.StatusBadGateway, "failed to enqueue build")
	}
	chatopsCommands.WithLabelValues("rerun", "ok").Inc()
	log.Printf("chatops rerun build=%d from=%d chat=%s", build.ID, source.ID, c.Locals("chatID"))
	return c.Status(fiber.StatusAccepted).JSON(build)
}

func (a *app) chatopsRollback(c *fiber.Ctx) error {
	var body struct {
		Repository string `json:"repository"`
	}
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&body); err != nil {
			return jsonError(c, fiber.StatusBadRequest, "invalid json body")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	repository := strings.TrimSpace(body.Repository)
	if repository == "" {
		names, err := a.store.ReleaseRepositories(ctx)
		if err != nil {
			chatopsCommands.WithLabelValues("rollback", "error").Inc()
			return jsonError(c, fiber.StatusInternalServerError, "failed to list releases")
		}
		if len(names) != 1 {
			return jsonError(c, fiber.StatusBadRequest, "repository is required")
		}
		repository = names[0]
	}

	job, err := a.store.CreateRollback(ctx, repository, c.Locals("chatID").(string))
	if errors.Is(err, store.ErrNoRelease) {
		return jsonError(c, fiber.StatusConflict, "no active release to roll back to")
	}
	if errors.Is(err, store.ErrJobPending) {
		return jsonError(c, fiber.StatusConflict, "a rollback is already in progress")
	}
	if err != nil {
		chatopsCommands.WithLabelValues("rollback", "error").Inc()
		log.Printf("chatops rollback %s: %v", repository, err)
		return jsonError(c, fiber.StatusInternalServerError, "failed to queue rollback")
	}
	chatopsCommands.WithLabelValues("rollback", "ok").Inc()
	log.Printf("chatops rollback job=%d repository=%s", job.ID, repository)
	return c.Status(fiber.StatusAccepted).JSON(job)
}

func (a *app) chatopsJob(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return jsonError(c, fiber.StatusBadRequest, "invalid job id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	job, err := a.store.DeployJob(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return jsonError(c, fiber.StatusNotFound, "job not found")
	}
	if err != nil {
		return jsonError(c, fiber.StatusInternalServerError, "failed to load job")
	}
	return c.JSON(job)
}
