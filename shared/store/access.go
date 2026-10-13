package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const sessionTTL = 14 * 24 * time.Hour

var (
	ErrNoSession     = errors.New("session not found")
	ErrEmailTaken    = errors.New("email already used")
	ErrRoleNameTaken = errors.New("role name already used")
	ErrRoleInUse     = errors.New("role is assigned to a user")
	ErrLastAdmin     = errors.New("last admin")
	ErrSystemRole    = errors.New("system role")
)

// Permission is one capability a role can grant.
type Permission struct {
	Key   string
	Label string
	Group string
}

// PermissionCatalog is the single list of capabilities. System roles are
// reset to these keys on startup.
func PermissionCatalog() []Permission {
	return []Permission{
		{Key: "builds.read", Label: "Melihat dasbor, build, dan log", Group: "Build"},
		{Key: "builds.rerun", Label: "Menjalankan ulang build", Group: "Build"},
		{Key: "users.read", Label: "Melihat pengguna", Group: "Pengguna"},
		{Key: "users.write", Label: "Mengubah pengguna", Group: "Pengguna"},
		{Key: "roles.read", Label: "Melihat peran", Group: "Peran"},
		{Key: "roles.write", Label: "Mengubah peran", Group: "Peran"},
		{Key: "integrations.read", Label: "Melihat integrasi", Group: "Integrasi"},
		{Key: "integrations.test", Label: "Menguji koneksi integrasi", Group: "Integrasi"},
	}
}

func systemRoles() map[string]struct {
	description string
	perms       []string
} {
	all := make([]string, 0, len(PermissionCatalog()))
	for _, p := range PermissionCatalog() {
		all = append(all, p.Key)
	}
	return map[string]struct {
		description string
		perms       []string
	}{
		"admin":    {"Akses penuh, termasuk pengguna dan peran.", all},
		"operator": {"Menjalankan build dan menguji integrasi.", []string{"builds.read", "builds.rerun", "integrations.read", "integrations.test"}},
		"viewer":   {"Melihat build dan status integrasi.", []string{"builds.read", "integrations.read"}},
	}
}

// Account is the signed-in user and the permissions of their role.
type Account struct {
	ID       int64
	Email    string
	Name     string
	RoleID   int64
	RoleName string
	Disabled bool
	perms    map[string]struct{}
}

func (a Account) Can(permission string) bool {
	if a.Disabled {
		return false
	}
	_, ok := a.perms[permission]
	return ok
}

// WithPerms returns a copy that can perform the given keys. Session loading
// fills permissions from the database; this is for rendering tests.
func (a Account) WithPerms(keys ...string) Account {
	a.perms = make(map[string]struct{}, len(keys))
	for _, key := range keys {
		a.perms[key] = struct{}{}
	}
	return a
}

type UserRow struct {
	ID        int64
	Email     string
	Name      string
	RoleID    int64
	RoleName  string
	Disabled  bool
	CreatedAt time.Time
}

type Role struct {
	ID          int64
	Name        string
	Description string
	System      bool
	Permissions []string
	UserCount   int
	CreatedAt   time.Time
}

func (r Role) Has(permission string) bool {
	for _, p := range r.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

type LoginUser struct {
	ID           int64
	PasswordHash string
	Disabled     bool
}

// EnsureAccess inserts permissions and system roles, then creates the first
// admin when the console has no users yet. Existing admins are left as they are.
func (s *Store) EnsureAccess(ctx context.Context, email, name, passwordHash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, p := range PermissionCatalog() {
		if _, err := tx.Exec(ctx, `INSERT INTO permissions (key) VALUES ($1) ON CONFLICT DO NOTHING`, p.Key); err != nil {
			return err
		}
	}
	for roleName, spec := range systemRoles() {
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO roles (name, description, system) VALUES ($1, $2, TRUE)
			ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description, system = TRUE
			RETURNING id`, roleName, spec.description).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id = $1`, id); err != nil {
			return err
		}
		for _, perm := range spec.perms {
			if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, permission) VALUES ($1, $2)`, id, perm); err != nil {
				return err
			}
		}
	}

	var users int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	if users == 0 {
		var adminID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM roles WHERE name = 'admin'`).Scan(&adminID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (email, name, password_hash, role_id) VALUES ($1, $2, $3, $4)`,
			email, name, passwordHash, adminID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) UserForLogin(ctx context.Context, email string) (LoginUser, error) {
	var u LoginUser
	err := s.pool.QueryRow(ctx, `SELECT id, password_hash, disabled FROM users WHERE email = $1`, email).
		Scan(&u.ID, &u.PasswordHash, &u.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginUser{}, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateSession(ctx context.Context, id string, userID int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))`,
		id, userID, sessionTTL.Seconds())
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

func (s *Store) AccountBySession(ctx context.Context, sessionID string) (Account, error) {
	var a Account
	var perms []string
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.disabled, u.role_id, r.name,
			COALESCE(array_agg(rp.permission) FILTER (WHERE rp.permission IS NOT NULL), '{}')
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		JOIN roles r ON r.id = u.role_id
		LEFT JOIN role_permissions rp ON rp.role_id = r.id
		WHERE s.id = $1 AND s.expires_at > now()
		GROUP BY u.id, r.name`, sessionID).
		Scan(&a.ID, &a.Email, &a.Name, &a.Disabled, &a.RoleID, &a.RoleName, &perms)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNoSession
	}
	if err != nil {
		return Account{}, err
	}
	a.perms = map[string]struct{}{}
	for _, p := range perms {
		a.perms[p] = struct{}{}
	}
	if a.Disabled {
		return Account{}, ErrNoSession
	}
	return a, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.name, u.role_id, r.name, u.disabled, u.created_at
		FROM users u JOIN roles r ON r.id = u.role_id
		ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UserRow, error) {
		var u UserRow
		err := row.Scan(&u.ID, &u.Email, &u.Name, &u.RoleID, &u.RoleName, &u.Disabled, &u.CreatedAt)
		return u, err
	})
}

func (s *Store) GetUser(ctx context.Context, id int64) (UserRow, error) {
	var u UserRow
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.role_id, r.name, u.disabled, u.created_at
		FROM users u JOIN roles r ON r.id = u.role_id WHERE u.id = $1`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.RoleID, &u.RoleName, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash string, roleID int64) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash, role_id) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, name, passwordHash, roleID).Scan(&id)
	if isUnique(err) {
		return 0, ErrEmailTaken
	}
	return id, err
}

func (s *Store) UpdateUser(ctx context.Context, id int64, name string, roleID int64, disabled bool, passwordHash string) error {
	user, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if user.RoleName == "admin" && !user.Disabled && (disabled || roleID != user.RoleID) {
		others, err := s.otherAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if passwordHash == "" {
		_, err = s.pool.Exec(ctx, `UPDATE users SET name = $2, role_id = $3, disabled = $4 WHERE id = $1`,
			id, name, roleID, disabled)
	} else {
		_, err = s.pool.Exec(ctx, `UPDATE users SET name = $2, role_id = $3, disabled = $4, password_hash = $5 WHERE id = $1`,
			id, name, roleID, disabled, passwordHash)
	}
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	user, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if user.RoleName == "admin" && !user.Disabled {
		others, err := s.otherAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) otherAdmins(ctx context.Context, except int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM users u JOIN roles r ON r.id = u.role_id
		WHERE r.name = 'admin' AND NOT u.disabled AND u.id <> $1`, except).Scan(&n)
	return n, err
}

func (s *Store) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.name, r.description, r.system, r.created_at,
			(SELECT count(*) FROM users u WHERE u.role_id = r.id)
		FROM roles r ORDER BY r.system DESC, r.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Role, error) {
		var r Role
		err := row.Scan(&r.ID, &r.Name, &r.Description, &r.System, &r.CreatedAt, &r.UserCount)
		return r, err
	})
}

func (s *Store) GetRole(ctx context.Context, id int64) (Role, error) {
	var r Role
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, r.name, r.description, r.system, r.created_at,
			(SELECT count(*) FROM users u WHERE u.role_id = r.id),
			COALESCE(array_agg(rp.permission) FILTER (WHERE rp.permission IS NOT NULL), '{}')
		FROM roles r
		LEFT JOIN role_permissions rp ON rp.role_id = r.id
		WHERE r.id = $1
		GROUP BY r.id`, id).Scan(&r.ID, &r.Name, &r.Description, &r.System, &r.CreatedAt, &r.UserCount, &r.Permissions)
	if errors.Is(err, pgx.ErrNoRows) {
		return Role{}, ErrNotFound
	}
	return r, err
}

func (s *Store) CreateRole(ctx context.Context, name, description string, perms []string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO roles (name, description) VALUES ($1, $2) RETURNING id`, name, description).Scan(&id)
	if isUnique(err) {
		return 0, ErrRoleNameTaken
	}
	if err != nil {
		return 0, err
	}
	if err := insertPerms(ctx, tx, id, perms); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) UpdateRole(ctx context.Context, id int64, description string, perms []string) error {
	role, err := s.GetRole(ctx, id)
	if err != nil {
		return err
	}
	if role.System {
		return ErrSystemRole
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE roles SET description = $2 WHERE id = $1`, id, description); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_permissions WHERE role_id = $1`, id); err != nil {
		return err
	}
	if err := insertPerms(ctx, tx, id, perms); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteRole(ctx context.Context, id int64) error {
	role, err := s.GetRole(ctx, id)
	if err != nil {
		return err
	}
	if role.System {
		return ErrSystemRole
	}
	if role.UserCount > 0 {
		return ErrRoleInUse
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM roles WHERE id = $1 AND NOT system`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertPerms(ctx context.Context, tx execer, roleID int64, perms []string) error {
	for _, p := range perms {
		if _, err := tx.Exec(ctx, `INSERT INTO role_permissions (role_id, permission) VALUES ($1, $2)`, roleID, p); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
