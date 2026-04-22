// Package store/migrate.go
//
// Purpose:  Runs all pending database migrations automatically at app startup.
//           Checks which migrations have already been applied (tracked in the
//           schema_migrations table that golang-migrate manages automatically)
//           and applies only the new ones. Safe to call on every startup.
//
// Inputs:   dbURL string — the full Postgres connection string from config
//           migrationsPath string — path to the migrations/ directory
//
// Outputs:  error if any migration fails; nil if all applied or already up to date
//
// What calls this: cmd/server/main.go and cmd/worker/main.go, before any
//                  other DB operations. Both processes must run migrations
//                  because either might start first.
//
// What breaks if wrong: app starts against a schema that doesn't match the
//                       Go struct definitions — queries fail at runtime with
//                       cryptic column-not-found errors instead of a clear
//                       startup message.

package store

import (
	"errors"
	"fmt"
	"log"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers pgx5:// driver
	_ "github.com/golang-migrate/migrate/v4/source/file"     // registers file:// source
)

// RunMigrations applies all pending migrations from the migrations/ directory.
// It is idempotent — running it multiple times on an up-to-date schema is safe.
//
// migrationsPath must be an absolute path, e.g. "/app/migrations"
// dbURL must use the postgres:// scheme from config.DBURL.
//
// What can go wrong:
//   - migrations directory not found → clear error with path
//   - migration SQL has a syntax error → error names the failing file
//   - DB connection fails → error with connection details
//   - already up to date → not an error, logged as info
func RunMigrations(dbURL, migrationsPath string) error {
	// golang-migrate's pgx/v5 driver uses the pgx5:// scheme.
	// We receive a postgres:// URL from config and convert the scheme here.
	// The rest of the connection string (host, user, password, dbname) is identical.
	pgx5URL := "pgx5" + dbURL[len("postgres"):]

	// file:// source tells golang-migrate to read migration files from disk.
	// Path must point to the directory containing *.up.sql and *.down.sql files.
	sourceURL := fmt.Sprintf("file://%s", migrationsPath)

	m, err := migrate.New(sourceURL, pgx5URL)
	if err != nil {
		return fmt.Errorf("migrate.New: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			log.Printf("migrate: source close error: %v", srcErr)
		}
		if dbErr != nil {
			log.Printf("migrate: db close error: %v", dbErr)
		}
	}()

	// Up() applies all pending migrations in version order.
	// migrate.ErrNoChange means the schema is already up to date — not an error.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate.Up: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("migrate.Version: %w", err)
	}

	if dirty {
		// A dirty state means a migration failed mid-run.
		// The schema is in an unknown state — manual intervention required.
		return fmt.Errorf("migrate: schema is dirty at version %d — fix the migration and run again", version)
	}

	log.Printf("migrate: schema at version %d — all migrations applied", version)
	return nil
}
