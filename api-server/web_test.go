package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"devops-allinone/shared/store"
)

func TestParseViews(t *testing.T) {
	views, err := parseViews()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"landing", "login", "dashboard", "builds", "build", "users", "userform", "roles", "roleform", "integrations"} {
		if views.Lookup(name) == nil {
			t.Errorf("template %s missing", name)
		}
	}
}

func TestExecuteViews(t *testing.T) {
	views, err := parseViews()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	admin := store.Account{ID: 1, Name: "Admin", Email: "admin@opsora.local", RoleName: "admin"}.WithPerms(
		"builds.read", "builds.rerun", "users.read", "users.write",
		"roles.read", "roles.write", "integrations.read", "integrations.test",
	)
	p := page{
		Title: "Uji", CSRF: "token", Next: "/app",
		Account: &admin,
		Totals:  store.BuildTotals{Success: 2, Failed: 1, Running: 1, Queued: 0},
		Releases: []store.Release{{
			ID: 3, Repository: "bionte4/opsora", CommitSHA: "abcdef1234567890", BuildID: 7, Status: "active",
		}},
		Jobs: []store.DeployJob{{
			Repository: "bionte4/opsora", Action: "rollback", Status: "running", CreatedAt: now,
		}},
		Notes: []store.AnalysisNote{{
			BuildID: 7, Repository: "bionte4/opsora", Summary: "tes gagal", CreatedAt: now, Repeated: true,
		}},
		Builds: []store.Build{{
			ID: 7, Repository: "bionte4/opsora", Ref: "refs/heads/main",
			CommitSHA: "abcdef1234567890", Status: "success", Actor: "ada", CreatedAt: now,
		}},
		Build: store.Build{
			ID: 7, Repository: "bionte4/opsora", Ref: "refs/heads/main",
			CommitSHA: "abcdef1234567890", Status: "failed", Actor: "ada", Error: "exit 1",
			Steps: []store.Step{{Name: "test", Status: "failed"}},
		},
		Logs:     []store.LogLine{{Line: "FAIL"}},
		Analysis: &store.Analysis{Model: "phi3", Summary: "tes gagal", Cause: "assert", Lines: []store.LogLine{{Line: "FAIL"}}},
		Users:    []store.UserRow{{ID: 1, Name: "Admin", Email: "admin@opsora.local", RoleName: "admin"}},
		User:     store.UserRow{ID: 2, Name: "Bea", Email: "bea@opsora.local", RoleID: 2},
		Roles:    []store.Role{{ID: 1, Name: "admin", Description: "penuh", System: true, UserCount: 1}},
		Role:     store.Role{ID: 4, Name: "release", Description: "rilis", Permissions: []string{"builds.read"}},
		Groups:   permissionGroups(),
	}
	for _, name := range []string{"landing", "login", "dashboard", "builds", "build", "users", "userform", "roles", "roleform", "integrations"} {
		var buf bytes.Buffer
		if err := views.ExecuteTemplate(&buf, name, p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		html := buf.String()
		if !strings.Contains(html, "Opsora") {
			t.Errorf("%s rendered without the product name", name)
		}
		if name == "dashboard" {
			for _, want := range []string{"Dasbor", "Pipeline terbaru", "Pola berulang", "Buka integrasi", ">2<"} {
				if !strings.Contains(html, want) {
					t.Errorf("dashboard missing %q", want)
				}
			}
		}
		if name == "builds" && !strings.Contains(html, "/app/builds/7") {
			t.Error("build list missing the row link")
		}
	}
}

func TestBearerMatch(t *testing.T) {
	if bearerMatch("Bearer secret", "") || bearerMatch("Bearer nope", "secret") {
		t.Fatal("wrong token must not match")
	}
	if !bearerMatch("Bearer secret", "secret") {
		t.Fatal("expected a match")
	}
}

func TestSafeNext(t *testing.T) {
	if safeNext("/app/users") != "/app/users" {
		t.Fatal("relative path should pass")
	}
	for _, raw := range []string{"https://evil.test", "//evil.test", "app", ""} {
		if safeNext(raw) != "/app" {
			t.Errorf("safeNext(%q) should fall back to /app", raw)
		}
	}
}

func TestAccountPermissions(t *testing.T) {
	admin := store.Account{}
	if admin.Can("users.write") {
		t.Fatal("empty account has no permissions")
	}
	// Can is false when disabled even if a permission was granted.
	disabled := store.Account{Disabled: true}
	if disabled.Can("builds.read") {
		t.Fatal("disabled account must not pass")
	}
	if !strings.Contains(store.PermissionCatalog()[0].Key, ".") {
		t.Fatal("catalog keys should be grouped")
	}
}
