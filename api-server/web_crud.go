package main

import (
	"context"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"

	"devops-allinone/shared/store"

	"github.com/gofiber/fiber/v2"
)

var roleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

func (a *app) pageUsers(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	users, err := a.store.ListUsers(ctx)
	p := page{Title: "Pengguna", Nav: "users", Users: users}
	if err != nil {
		log.Printf("list users: %v", err)
		p.Error = "Daftar pengguna gagal dimuat."
	}
	return a.render(c, "users", p)
}

func (a *app) pageUserNew(c *fiber.Ctx) error {
	p, err := a.userForm(c, page{Title: "Pengguna baru", Nav: "users", IsNew: true})
	if err != nil {
		return err
	}
	return a.render(c, "userform", p)
}

func (a *app) pageUserEdit(c *fiber.Ctx) error {
	id, ok := pathID(c)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("Pengguna tidak ditemukan.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	user, err := a.store.GetUser(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString("Pengguna tidak ditemukan.")
	}
	if err != nil {
		log.Printf("get user %d: %v", id, err)
		return c.Status(fiber.StatusInternalServerError).SendString("Pengguna gagal dimuat.")
	}
	p, err := a.userForm(c, page{Title: "Ubah pengguna", Nav: "users", User: user})
	if err != nil {
		return err
	}
	return a.render(c, "userform", p)
}

func (a *app) submitUser(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	isNew := c.Path() == "/app/users"
	var id int64
	if !isNew {
		var ok bool
		id, ok = pathID(c)
		if !ok {
			return c.Status(fiber.StatusNotFound).SendString("Pengguna tidak ditemukan.")
		}
	}
	name := strings.TrimSpace(c.FormValue("name"))
	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	password := c.FormValue("password")
	roleID, _ := strconv.ParseInt(c.FormValue("role_id"), 10, 64)
	disabled := c.FormValue("disabled") == "1"

	p := page{Title: "Pengguna", Nav: "users", IsNew: isNew, User: store.UserRow{ID: id, Name: name, Email: email, RoleID: roleID, Disabled: disabled}}
	var err error
	p, err = a.userForm(c, p)
	if err != nil {
		return err
	}
	fail := func(msg string) error {
		p.Error = msg
		return a.render(c, "userform", p)
	}
	if name == "" || len(name) > 80 {
		return fail("Nama wajib diisi, paling panjang 80 karakter.")
	}
	if isNew && !validEmail(email) {
		return fail("Email tidak valid.")
	}
	if (isNew || password != "") && (len(password) < 8 || len(password) > 200) {
		return fail("Kata sandi minimal 8 karakter.")
	}
	if roleID < 1 {
		return fail("Pilih peran.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	var hash string
	if password != "" {
		hash, err = hashPassword(password)
		if err != nil {
			return fail("Kata sandi gagal disimpan.")
		}
	}
	if isNew {
		if _, err := a.store.CreateUser(ctx, email, name, hash, roleID); errors.Is(err, store.ErrEmailTaken) {
			return fail("Email itu sudah dipakai.")
		} else if err != nil {
			log.Printf("create user: %v", err)
			return fail("Pengguna gagal disimpan.")
		}
		a.flash(c, "Pengguna dibuat.")
		return c.Redirect("/app/users", fiber.StatusSeeOther)
	}
	if err := a.store.UpdateUser(ctx, id, name, roleID, disabled, hash); errors.Is(err, store.ErrLastAdmin) {
		return fail("Admin terakhir tidak boleh diturunkan atau dinonaktifkan.")
	} else if errors.Is(err, store.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString("Pengguna tidak ditemukan.")
	} else if err != nil {
		log.Printf("update user %d: %v", id, err)
		return fail("Pengguna gagal disimpan.")
	}
	a.flash(c, "Pengguna disimpan.")
	return c.Redirect("/app/users", fiber.StatusSeeOther)
}

func (a *app) deleteUser(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	id, ok := pathID(c)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("Pengguna tidak ditemukan.")
	}
	acc := c.Locals("account").(*store.Account)
	if acc.ID == id {
		a.flash(c, "Anda tidak bisa menghapus akun yang sedang dipakai.")
		return c.Redirect("/app/users/"+c.Params("id"), fiber.StatusSeeOther)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := a.store.DeleteUser(ctx, id); errors.Is(err, store.ErrLastAdmin) {
		a.flash(c, "Admin terakhir tidak bisa dihapus.")
		return c.Redirect("/app/users/"+c.Params("id"), fiber.StatusSeeOther)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("delete user %d: %v", id, err)
		a.flash(c, "Pengguna gagal dihapus.")
		return c.Redirect("/app/users", fiber.StatusSeeOther)
	}
	a.flash(c, "Pengguna dihapus.")
	return c.Redirect("/app/users", fiber.StatusSeeOther)
}

func (a *app) pageRoles(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	roles, err := a.store.ListRoles(ctx)
	p := page{Title: "Peran", Nav: "roles", Roles: roles}
	if err != nil {
		log.Printf("list roles: %v", err)
		p.Error = "Daftar peran gagal dimuat."
	}
	return a.render(c, "roles", p)
}

func (a *app) pageRoleNew(c *fiber.Ctx) error {
	return a.render(c, "roleform", page{Title: "Peran baru", Nav: "roles", IsNew: true, Groups: permissionGroups()})
}

func (a *app) pageRoleEdit(c *fiber.Ctx) error {
	id, ok := pathID(c)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("Peran tidak ditemukan.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	role, err := a.store.GetRole(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString("Peran tidak ditemukan.")
	}
	if err != nil {
		log.Printf("get role %d: %v", id, err)
		return c.Status(fiber.StatusInternalServerError).SendString("Peran gagal dimuat.")
	}
	return a.render(c, "roleform", page{Title: role.Name, Nav: "roles", Role: role, Groups: permissionGroups()})
}

func (a *app) submitRole(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	isNew := c.Path() == "/app/roles"
	name := strings.ToLower(strings.TrimSpace(c.FormValue("name")))
	description := strings.TrimSpace(c.FormValue("description"))
	perms := formPerms(c)
	p := page{
		Title: "Peran", Nav: "roles", IsNew: isNew, Groups: permissionGroups(),
		Role: store.Role{Name: name, Description: description, Permissions: perms},
	}
	fail := func(msg string) error {
		p.Error = msg
		return a.render(c, "roleform", p)
	}
	if len(description) > 200 {
		return fail("Deskripsi paling panjang 200 karakter.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if isNew {
		if !roleNamePattern.MatchString(name) || name == "admin" || name == "operator" || name == "viewer" {
			return fail("Nama peran: huruf kecil, angka, tanda hubung, dan bukan nama peran sistem.")
		}
		if _, err := a.store.CreateRole(ctx, name, description, perms); errors.Is(err, store.ErrRoleNameTaken) {
			return fail("Nama peran itu sudah dipakai.")
		} else if err != nil {
			log.Printf("create role: %v", err)
			return fail("Peran gagal disimpan.")
		}
		a.flash(c, "Peran dibuat.")
		return c.Redirect("/app/roles", fiber.StatusSeeOther)
	}
	id, ok := pathID(c)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("Peran tidak ditemukan.")
	}
	p.Role.ID = id
	if err := a.store.UpdateRole(ctx, id, description, perms); errors.Is(err, store.ErrSystemRole) {
		return fail("Peran sistem tidak bisa diubah.")
	} else if errors.Is(err, store.ErrNotFound) {
		return c.Status(fiber.StatusNotFound).SendString("Peran tidak ditemukan.")
	} else if err != nil {
		log.Printf("update role %d: %v", id, err)
		return fail("Peran gagal disimpan.")
	}
	a.flash(c, "Peran disimpan.")
	return c.Redirect("/app/roles", fiber.StatusSeeOther)
}

func (a *app) deleteRole(c *fiber.Ctx) error {
	if !a.csrfOK(c) {
		return a.rejectCSRF(c)
	}
	id, ok := pathID(c)
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("Peran tidak ditemukan.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	err := a.store.DeleteRole(ctx, id)
	switch {
	case errors.Is(err, store.ErrSystemRole):
		a.flash(c, "Peran sistem tidak bisa dihapus.")
		return c.Redirect("/app/roles/"+c.Params("id"), fiber.StatusSeeOther)
	case errors.Is(err, store.ErrRoleInUse):
		a.flash(c, "Peran masih dipakai pengguna.")
		return c.Redirect("/app/roles/"+c.Params("id"), fiber.StatusSeeOther)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		log.Printf("delete role %d: %v", id, err)
		a.flash(c, "Peran gagal dihapus.")
	default:
		a.flash(c, "Peran dihapus.")
	}
	return c.Redirect("/app/roles", fiber.StatusSeeOther)
}

func (a *app) userForm(c *fiber.Ctx, p page) (page, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	roles, err := a.store.ListRoles(ctx)
	if err != nil {
		log.Printf("roles for form: %v", err)
		return p, c.Status(fiber.StatusInternalServerError).SendString("Peran gagal dimuat.")
	}
	p.Roles = roles
	return p, nil
}

func pathID(c *fiber.Ctx) (int64, bool) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	return id, err == nil && id > 0
}

func validEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t") && len(s) <= 200
}

func formPerms(c *fiber.Ctx) []string {
	allow := map[string]bool{}
	for _, p := range store.PermissionCatalog() {
		allow[p.Key] = true
	}
	var out []string
	seen := map[string]bool{}
	c.Request().PostArgs().VisitAll(func(k, v []byte) {
		if string(k) != "perm" {
			return
		}
		key := string(v)
		if allow[key] && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	})
	return out
}
