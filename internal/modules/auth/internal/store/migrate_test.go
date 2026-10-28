package store

import (
	"bytes"
	"context"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"paygate/internal/contracts/auth"
	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/platform/database"
)

// The schema as the original SQL migration created it, with data in it.
const legacySchema = `
CREATE TABLE users (id UUID PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
  password_hash TEXT NOT NULL, merchant_id UUID, role TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('active', 'disabled')), failed_logins INT NOT NULL DEFAULT 0,
  locked_until TIMESTAMPTZ, last_login_at TIMESTAMPTZ, created_by UUID,
  created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
CREATE INDEX users_merchant_idx ON users (merchant_id);
CREATE TABLE sessions (id UUID PRIMARY KEY, user_id UUID NOT NULL REFERENCES users (id),
  token_hash TEXT NOT NULL UNIQUE, expires_at TIMESTAMPTZ NOT NULL, revoked_at TIMESTAMPTZ,
  replaced_by UUID, created_at TIMESTAMPTZ NOT NULL);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE TABLE api_keys (id UUID PRIMARY KEY, merchant_id UUID NOT NULL, name TEXT NOT NULL,
  prefix TEXT NOT NULL, last4 TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, pepper_version INT NOT NULL,
  environment TEXT NOT NULL CHECK (environment IN ('live', 'test')), scopes JSONB NOT NULL,
  expires_at TIMESTAMPTZ, revoked_at TIMESTAMPTZ, last_used_at TIMESTAMPTZ, created_by UUID,
  created_at TIMESTAMPTZ NOT NULL);
CREATE INDEX api_keys_merchant_idx ON api_keys (merchant_id);
CREATE TABLE merchants (id UUID PRIMARY KEY, status TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL);

INSERT INTO users VALUES ('11111111-1111-1111-1111-111111111111', 'owner@old.test', 'Old Owner', 'x',
  '22222222-2222-2222-2222-222222222222', 'owner', 'active', 0, NULL, NULL, NULL, now(), now());
INSERT INTO api_keys VALUES ('33333333-3333-3333-3333-333333333333', '22222222-2222-2222-2222-222222222222',
  'k', 'sk_live_abcd', 'wxyz', 'hash1', 1, 'live', '["payments:read"]', NULL, NULL, NULL, NULL, now());
`

// TestAutoMigrateUpgradesWithoutLoss: AutoMigrate adopts a database created by
// the old SQL migration, adds the new RBAC tables, keeps every row, and a
// second run changes nothing.
func TestAutoMigrateUpgradesWithoutLoss(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	const schema = "auth_upgrade_test"
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	db, err := database.Open(url+sep+"search_path="+schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	defer db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	if err := db.Exec("CREATE SCHEMA " + schema + "; SET search_path TO " + schema + ";" + legacySchema).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.Migrate(ctx, db, schema, Models()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var n int64
	db.Raw("SELECT count(*) FROM users WHERE email = 'owner@old.test' AND role = 'owner'").Scan(&n)
	if n != 1 {
		t.Fatal("existing user lost")
	}
	db.Raw("SELECT count(*) FROM api_keys WHERE hash = 'hash1'").Scan(&n)
	if n != 1 {
		t.Fatal("existing API key lost")
	}
	for _, table := range []string{"permissions", "roles", "role_permissions", "user_roles", "audit_log"} {
		db.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?", schema, table).Scan(&n)
		if n != 1 {
			t.Fatalf("table %s not created", table)
		}
	}
	db.Raw("SELECT count(*) FROM pg_indexes WHERE schemaname = ? AND tablename = 'users'", schema).Scan(&n)
	if n != 3 { // pkey, users_email_key, users_merchant_idx: existing indexes adopted, not duplicated
		t.Fatalf("users has %d indexes, want 3", n)
	}

	// Existing users get the built-in role matching their old role column.
	st := New(db)
	now := time.Now()
	for _, sr := range domain.SystemRoles {
		if err := st.EnsureRole(ctx, sr.Role(now)); err != nil {
			t.Fatal(err)
		}
	}
	if added, err := st.BackfillUserRoles(ctx, now); err != nil || added != 1 {
		t.Fatalf("backfill: %d %v", added, err)
	}
	roles, err := st.UserRoles(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil || len(roles) != 1 || roles[0].Name != auth.RoleOwner || !roles[0].AllPermissions {
		t.Fatalf("backfilled roles: %v %v", roles, err)
	}
	if added, _ := st.BackfillUserRoles(ctx, now); added != 0 {
		t.Fatal("backfill must be idempotent")
	}

	// Removing a unique tag would make GORM drop the constraint: refused.
	type userWithoutUnique struct {
		ID    string `gorm:"type:uuid;primaryKey"`
		Email string `gorm:"type:text;not null"`
	}
	err = database.Migrate(ctx, db.Table("users"), schema, []any{&userWithoutUnique{}})
	if err == nil || !strings.Contains(err.Error(), "refused a destructive change") {
		t.Fatalf("want destructive change refused, got %v", err)
	}
	db.Raw("SELECT count(*) FROM pg_constraint WHERE conname = 'users_email_key'").Scan(&n)
	if n != 1 {
		t.Fatal("unique constraint was dropped")
	}

	// Second start: no schema change at all.
	var buf bytes.Buffer
	logged := db.Session(&gorm.Session{Logger: logger.New(log.New(&buf, "", 0), logger.Config{LogLevel: logger.Info})})
	if err := database.Migrate(ctx, logged, schema, Models()); err != nil {
		t.Fatal(err)
	}
	// (CREATE ... IF NOT EXISTS for the schema and history table is expected.)
	ddl := regexp.MustCompile(`(?i)(ALTER TABLE|DROP |CREATE TABLE "|CREATE (UNIQUE )?INDEX ")`)
	if found := ddl.FindAllString(buf.String(), -1); len(found) > 0 {
		t.Fatalf("second migration ran DDL %v:\n%s", found, buf.String())
	}
}

// TestAutoMigrateFreshIsStable: on a new database the second start is a no-op.
func TestAutoMigrateFreshIsStable(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	const schema = "auth_fresh_test"
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	db, err := database.Open(url+sep+"search_path="+schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	defer db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	if err := database.Migrate(ctx, db, schema, Models()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logged := db.Session(&gorm.Session{Logger: logger.New(log.New(&buf, "", 0), logger.Config{LogLevel: logger.Info})})
	if err := database.Migrate(ctx, logged, schema, Models()); err != nil {
		t.Fatal(err)
	}
	ddl := regexp.MustCompile(`(?i)(ALTER TABLE|DROP |CREATE TABLE "|CREATE (UNIQUE )?INDEX ")`)
	if found := ddl.FindAllString(buf.String(), -1); len(found) > 0 {
		t.Fatalf("second migration ran DDL %v:\n%s", found, buf.String())
	}
}
