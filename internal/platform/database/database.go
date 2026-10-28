// Package database opens one GORM connection pool per module and migrates
// each module's own schema.
//
// Two kinds of migration, both run on every start, in one transaction:
//
//  1. SQL files (*.sql): applied once each, in name order. Used for things an
//     ORM cannot express (partial indexes, data fixes).
//  2. GORM AutoMigrate of the module's models: creates missing tables,
//     columns and indexes, and widens column types. It is additive only: a
//     guard (see additiveOnly) rejects any DROP it would run, so it is safe on
//     every start and every replica. Renames and destructive changes need a
//     deliberate SQL file.
package database

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"regexp"
	"sort"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open creates a connection pool for one module. Keep pools small: the sum of
// all modules' pools must fit within Postgres max_connections.
func Open(dsn string, maxConns int) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
			SlowThreshold:             500 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
		}),
		NowFunc:        func() time.Time { return time.Now().UTC() },
		TranslateError: true, // unique violations -> gorm.ErrDuplicatedKey
	})
	if err != nil {
		return nil, err
	}
	if err := db.Callback().Raw().Before("gorm:raw").Register("paygate:additive_only", additiveOnly); err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(max(1, maxConns/2))
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database ping: %w", err)
	}
	return db, nil
}

var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Migrate applies *.sql files (from all sources, sorted by file name) that have
// not been applied yet, then auto-migrates models. Everything runs in one
// transaction guarded by an advisory lock, so several replicas can start at
// the same time safely. The migration history table lives in the module's own
// schema, so it moves together with the module when the module gets its own
// database.
func Migrate(ctx context.Context, db *gorm.DB, schema string, models []any, sources ...fs.FS) error {
	if !identRe.MatchString(schema) {
		return fmt.Errorf("invalid schema name %q", schema)
	}

	type file struct {
		name string
		src  fs.FS
	}
	var files []file
	seen := map[string]bool{}
	for _, src := range sources {
		if src == nil {
			continue
		}
		names, err := fs.Glob(src, "*.sql")
		if err != nil {
			return err
		}
		for _, n := range names {
			if seen[n] {
				return fmt.Errorf("duplicate migration %s in schema %s", n, schema)
			}
			seen[n] = true
			files = append(files, file{n, src})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "migrate:"+schema).Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE SCHEMA IF NOT EXISTS " + schema).Error; err != nil {
			return err
		}
		if err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`).Error; err != nil {
			return err
		}
		var applied []string
		if err := tx.Raw("SELECT version FROM schema_migrations").Scan(&applied).Error; err != nil {
			return err
		}
		done := map[string]bool{}
		for _, v := range applied {
			done[v] = true
		}
		for _, f := range files {
			if done[f.name] {
				continue
			}
			body, err := fs.ReadFile(f.src, f.name)
			if err != nil {
				return err
			}
			if err := tx.Exec(string(body)).Error; err != nil {
				return fmt.Errorf("migration %s/%s: %w", schema, f.name, err)
			}
			if err := tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", f.name).Error; err != nil {
				return err
			}
		}
		if len(models) > 0 {
			actx := context.WithValue(ctx, autoMigrating{}, true)
			if err := tx.WithContext(actx).AutoMigrate(models...); err != nil {
				return fmt.Errorf("auto-migrate %s: %w", schema, err)
			}
		}
		return nil
	})
}

type autoMigrating struct{}

var destructiveDDL = regexp.MustCompile(`(?i)\bDROP\s+(TABLE|COLUMN|CONSTRAINT|INDEX|SCHEMA)\b`)

// additiveOnly blocks destructive statements issued by AutoMigrate (GORM may,
// for example, drop a unique constraint when a tag is removed). The migration
// then fails loudly and nothing is lost; the change has to be written as a
// SQL file on purpose.
func additiveOnly(tx *gorm.DB) {
	if tx.Statement.Context == nil || tx.Statement.Context.Value(autoMigrating{}) == nil {
		return
	}
	if q := tx.Statement.SQL.String(); destructiveDDL.MatchString(q) {
		_ = tx.AddError(fmt.Errorf("auto-migration refused a destructive change (%s); write a SQL migration for it", q))
	}
}

// StringList stores a []string as a JSONB array.
type StringList []string

func (s StringList) Value() (driver.Value, error) {
	if s == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(s))
	return string(b), err
}

func (s *StringList) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*s = nil
		return nil
	case []byte:
		return json.Unmarshal(t, (*[]string)(s))
	case string:
		return json.Unmarshal([]byte(t), (*[]string)(s))
	default:
		return fmt.Errorf("StringList: unsupported type %T", v)
	}
}

// StringMap stores a map[string]string as a JSONB object (e.g. metadata).
type StringMap map[string]string

func (m StringMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(m))
	return string(b), err
}

func (m *StringMap) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*m = nil
		return nil
	case []byte:
		return json.Unmarshal(t, (*map[string]string)(m))
	case string:
		return json.Unmarshal([]byte(t), (*map[string]string)(m))
	default:
		return fmt.Errorf("StringMap: unsupported type %T", v)
	}
}
