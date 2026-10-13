package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"devops-allinone/shared/queue"
	"devops-allinone/shared/store"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"golang.org/x/crypto/bcrypt"
)

//go:embed web/templates/*.html
var templateFS embed.FS

//go:embed web/static/*
var staticFS embed.FS

const sessionCookie = "opsora_session"

var dummyHash []byte

func init() {
	var err error
	dummyHash, err = bcrypt.GenerateFromPassword([]byte("opsora-not-a-user"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
}

type permGroup struct {
	Name  string
	Items []store.Permission
}

type page struct {
	Title    string
	Account  *store.Account
	Flash    string
	CSRF     string
	Nav      string
	Next     string
	Error    string
	Builds   []store.Build
	Build    store.Build
	Logs     []store.LogLine
	Analysis *store.Analysis
	Similar  []store.SimilarFailure
	Users    []store.UserRow
	User     store.UserRow
	Roles    []store.Role
	Role     store.Role
	Groups   []permGroup
	IsNew    bool
	Totals   store.BuildTotals
	Releases []store.Release
	Jobs     []store.DeployJob
	Notes    []store.AnalysisNote
}

func parseViews() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		"can": func(a *store.Account, permission string) bool {
			return a != nil && a.Can(permission)
		},
		"shortSHA": func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		},
		"refName": func(ref string) string {
			ref = strings.TrimPrefix(ref, "refs/heads/")
			return strings.TrimPrefix(ref, "refs/tags/")
		},
		"when": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format("02 Jan 2006 15:04")
		},
		"statusLabel": func(s string) string {
			switch s {
			case "queued":
				return "antre"
			case "running":
				return "berjalan"
			case "success":
				return "lulus"
			case "failed":
				return "gagal"
			case "timed_out":
				return "waktu habis"
			case "cancelled":
				return "dibatalkan"
			case "pending":
				return "menunggu"
			case "active":
				return "aktif"
			case "superseded":
				return "diganti"
			case "rolled_back":
				return "dikembalikan"
			case "done":
				return "selesai"
			default:
				return s
			}
		},
	}).ParseFS(templateFS, "web/templates/*.html")
}

func mountStatic(fiberApp *fiber.App) error {
	sub, err := fs.Sub(staticFS, "web/static")
	if err != nil {
		return err
	}
	fiberApp.Use("/static", filesystem.New(filesystem.Config{
		Root:   http.FS(sub),
		Browse: false,
		MaxAge: 3600,
	}))
	return nil
}

func (a *app) render(c *fiber.Ctx, name string, p page) error {
	if p.Account == nil {
		if acc, ok := c.Locals("account").(*store.Account); ok {
			p.Account = acc
		}
	}
	if p.CSRF == "" {
		p.CSRF = a.csrf(c)
	}
	if p.Flash == "" {
		p.Flash = a.takeFlash(c)
	}
	var buf bytes.Buffer
	if err := a.views.ExecuteTemplate(&buf, name, p); err != nil {
		log.Printf("template %s: %v", name, err)
		return c.Status(fiber.StatusInternalServerError).SendString("Halaman gagal dirender.")
	}
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	return c.Send(buf.Bytes())
}

func (a *app) pageLanding(c *fiber.Ctx) error {
	p := page{Account: a.optionalAccount(c)}
	return a.render(c, "landing", p)
}

func (a *app) pageLogin(c *fiber.Ctx) error {
	if a.optionalAccount(c) != nil {
		return c.Redirect("/app", fiber.StatusSeeOther)
	}
	return a.render(c, "login", page{Next: safeNext(c.Query("next"))})
}

func (a *app) submitLogin(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	password := c.FormValue("password")
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	user, err := a.store.UserForLogin(ctx, email)
	hash := user.PasswordHash
	if err != nil {
		hash = string(dummyHash)
	}
	match := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	if err != nil || !match || user.Disabled {
		return a.render(c, "login", page{Error: "Email atau kata sandi salah.", Next: safeNext(c.FormValue("next"))})
	}
	sid, err := randomToken()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("Gagal membuat sesi.")
	}
	if err := a.store.CreateSession(ctx, sid, user.ID); err != nil {
		log.Printf("create session: %v", err)
		return c.Status(fiber.StatusInternalServerError).SendString("Gagal membuat sesi.")
	}
	a.setSession(c, sid)
	return c.Redirect(safeNext(c.FormValue("next")), fiber.StatusSeeOther)
}

func (a *app) submitLogout(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	if sid := c.Cookies(sessionCookie); sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		_ = a.store.DeleteSession(ctx, sid)
	}
	c.Cookie(&fiber.Cookie{Name: sessionCookie, Value: "", Path: "/", Expires: time.Unix(0, 0), HTTPOnly: true})
	return c.Redirect("/", fiber.StatusSeeOther)
}

func (a *app) requireUser(c *fiber.Ctx) error {
	acc := a.optionalAccount(c)
	if acc == nil {
		next := url.QueryEscape(c.OriginalURL())
		return c.Redirect("/login?next="+next, fiber.StatusSeeOther)
	}
	c.Locals("account", acc)
	return c.Next()
}

func (a *app) requirePerm(permission string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		acc := c.Locals("account").(*store.Account)
		if !acc.Can(permission) {
			a.flash(c, "Anda tidak punya izin untuk halaman itu.")
			return c.Redirect("/app", fiber.StatusSeeOther)
		}
		return c.Next()
	}
}

func (a *app) pageDashboard(c *fiber.Ctx) error {
	p := page{Title: "Dasbor", Nav: "dashboard"}
	if p.Account == nil {
		p.Account = c.Locals("account").(*store.Account)
	}
	if !p.Account.Can("builds.read") {
		return a.render(c, "dashboard", p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var err error
	p.Totals, err = a.store.BuildTotalsSince(ctx, time.Now().Add(-24*time.Hour))
	a.dashboardErr(&p, "totals", err)
	p.Builds, err = a.store.ListBuilds(ctx, 8)
	a.dashboardErr(&p, "builds", err)
	p.Releases, err = a.store.ActiveReleases(ctx, 8)
	a.dashboardErr(&p, "releases", err)
	p.Jobs, err = a.store.OpenDeployJobs(ctx, 8)
	a.dashboardErr(&p, "jobs", err)
	p.Notes, err = a.store.RecentAnalyses(ctx, 3)
	a.dashboardErr(&p, "analyses", err)
	return a.render(c, "dashboard", p)
}

func (a *app) dashboardErr(p *page, what string, err error) {
	if err == nil {
		return
	}
	log.Printf("dashboard %s: %v", what, err)
	if p.Error == "" {
		p.Error = "Sebagian dasbor gagal dimuat."
	}
}

func (a *app) pageBuilds(c *fiber.Ctx) error {
	p := page{Title: "Build", Nav: "builds"}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	builds, err := a.store.ListBuilds(ctx, 20)
	if err != nil {
		log.Printf("builds: %v", err)
		p.Error = "Daftar build gagal dimuat."
	}
	p.Builds = builds
	return a.render(c, "builds", p)
}

func (a *app) pageBuild(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return c.Status(fiber.StatusNotFound).SendString("Build tidak ditemukan.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	build, err := a.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString("Build tidak ditemukan.")
	}
	if err != nil {
		log.Printf("build page %d: %v", id, err)
		return c.Status(fiber.StatusInternalServerError).SendString("Build gagal dimuat.")
	}
	logs, err := a.store.TailBuildLogs(ctx, id, 80)
	if err != nil {
		log.Printf("build logs %d: %v", id, err)
	}
	p := page{Title: "Build", Nav: "builds", Build: build, Logs: logs}
	if analysis, err := a.store.Analysis(ctx, id); err == nil {
		p.Analysis = &analysis
	}
	if similar, err := a.store.SimilarFailures(ctx, id); err != nil {
		log.Printf("similar %d: %v", id, err)
	} else {
		p.Similar = similar
	}
	return a.render(c, "build", p)
}

func (a *app) requireAPI(permission string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if permission == "builds.read" && bearerMatch(c.Get("Authorization"), a.apiReadToken) {
			return c.Next()
		}
		acc := a.optionalAccount(c)
		if acc == nil {
			return jsonError(c, fiber.StatusUnauthorized, "sign in required")
		}
		if !acc.Can(permission) {
			return jsonError(c, fiber.StatusForbidden, "missing permission")
		}
		return c.Next()
	}
}

func bearerMatch(header, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	if len(header) != len(prefix)+len(token) || !strings.HasPrefix(header, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(token)) == 1
}

func (a *app) submitRerun(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id < 1 {
		return c.Status(fiber.StatusBadRequest).SendString("Build tidak valid.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	source, err := a.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString("Build tidak ditemukan.")
	}
	if err != nil || source.CommitSHA == "" {
		a.flash(c, "Build ini tidak bisa dijalankan ulang.")
		return c.Redirect("/app/builds/"+c.Params("id"), fiber.StatusSeeOther)
	}
	nonce, err := randomToken()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("Gagal membuat build.")
	}
	acc := c.Locals("account").(*store.Account)
	build, created, err := a.store.CreateBuild(ctx, store.NewBuild{
		DeliveryID: "rerun-" + nonce[:16],
		Event:      "rerun",
		Payload:    []byte(`{"source":"console"}`),
		Repository: source.Repository,
		Ref:        source.Ref,
		CommitSHA:  source.CommitSHA,
		Actor:      acc.Email,
	})
	if err != nil || !created {
		log.Printf("console rerun %d: created=%v err=%v", id, created, err)
		a.flash(c, "Build baru gagal dibuat.")
		return c.Redirect("/app/builds/"+c.Params("id"), fiber.StatusSeeOther)
	}
	if err := queue.Enqueue(ctx, a.redis, build.ID); err != nil {
		log.Printf("console enqueue %d: %v", build.ID, err)
		a.flash(c, "Build tercatat, tetapi gagal masuk antrean.")
		return c.Redirect("/app/builds/"+strconv.FormatInt(build.ID, 10), fiber.StatusSeeOther)
	}
	a.flash(c, "Build baru masuk antrean.")
	return c.Redirect("/app/builds/"+strconv.FormatInt(build.ID, 10), fiber.StatusSeeOther)
}

func (a *app) pageIntegrations(c *fiber.Ctx) error {
	return a.render(c, "integrations", page{Title: "Integrasi", Nav: "integrations"})
}

func (a *app) submitAITest(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"ok": false, "error": "Formulir kedaluwarsa. Muat ulang halaman."})
	}
	return a.ai.test(c)
}

func (a *app) optionalAccount(c *fiber.Ctx) *store.Account {
	sid := c.Cookies(sessionCookie)
	if sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	acc, err := a.store.AccountBySession(ctx, sid)
	if err != nil {
		return nil
	}
	return &acc
}

func (a *app) setSession(c *fiber.Ctx, id string) {
	c.Cookie(&fiber.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HTTPOnly: true,
		SameSite: "Lax", MaxAge: 14 * 24 * 3600,
	})
}

func (a *app) csrf(c *fiber.Ctx) string {
	if t := c.Cookies("opsora_csrf"); len(t) >= 32 {
		return t
	}
	t, err := randomToken()
	if err != nil {
		return ""
	}
	c.Cookie(&fiber.Cookie{
		Name: "opsora_csrf", Value: t, Path: "/", HTTPOnly: true,
		SameSite: "Lax", MaxAge: 14 * 24 * 3600,
	})
	return t
}

func (a *app) csrfOK(c *fiber.Ctx) bool {
	form := []byte(c.FormValue("csrf"))
	cookie := []byte(c.Cookies("opsora_csrf"))
	return len(form) > 0 && subtle.ConstantTimeCompare(form, cookie) == 1
}

func (a *app) rejectCSRF(c *fiber.Ctx) error {
	a.flash(c, "Formulir kedaluwarsa. Muat ulang halaman, lalu coba lagi.")
	if strings.HasPrefix(c.Path(), "/app") {
		return c.Redirect("/app", fiber.StatusSeeOther)
	}
	return c.Redirect("/login", fiber.StatusSeeOther)
}

func (a *app) flash(c *fiber.Ctx, msg string) {
	c.Cookie(&fiber.Cookie{
		Name: "opsora_flash", Value: url.QueryEscape(msg), Path: "/",
		HTTPOnly: true, SameSite: "Lax", MaxAge: 60,
	})
}

func (a *app) takeFlash(c *fiber.Ctx) string {
	raw := c.Cookies("opsora_flash")
	if raw == "" {
		return ""
	}
	c.Cookie(&fiber.Cookie{Name: "opsora_flash", Value: "", Path: "/", Expires: time.Unix(0, 0), HTTPOnly: true})
	msg, err := url.QueryUnescape(raw)
	if err != nil {
		return ""
	}
	return msg
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func safeNext(raw string) string {
	if len(raw) > 200 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "://") {
		return "/app"
	}
	return raw
}

func (a *app) ensureAdmin(ctx context.Context) error {
	email := strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL")))
	if email == "" {
		email = "admin@opsora.local"
	}
	password := os.Getenv("ADMIN_PASSWORD")
	if password == "" {
		password = "change-me"
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	if err := a.store.EnsureAccess(ctx, email, "Admin", hash); err != nil {
		return err
	}
	if password == "change-me" {
		log.Printf("console admin %s is using the default password; set ADMIN_PASSWORD", email)
	}
	return nil
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func permissionGroups() []permGroup {
	var groups []permGroup
	index := map[string]int{}
	for _, p := range store.PermissionCatalog() {
		i, ok := index[p.Group]
		if !ok {
			index[p.Group] = len(groups)
			groups = append(groups, permGroup{Name: p.Group})
			i = len(groups) - 1
		}
		groups[i].Items = append(groups[i].Items, p)
	}
	return groups
}
