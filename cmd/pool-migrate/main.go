package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pool-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var configPath, sqlitePath string
	var replaceTarget bool
	var timeout time.Duration
	flags.StringVar(&configPath, "config", "", "configuration file; PostgreSQL DSN may be supplied through CODEX_POOL_POSTGRES_DSN")
	flags.StringVar(&sqlitePath, "sqlite", "", "source SQLite database path (defaults to database_path)")
	flags.BoolVar(&replaceTarget, "replace-target", false, "authorize transactional replacement of all PostgreSQL application rows")
	flags.DurationVar(&timeout, "timeout", 30*time.Minute, "maximum migration duration")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load configuration: %v\n", err)
		return 1
	}
	if strings.TrimSpace(sqlitePath) == "" {
		sqlitePath = cfg.DatabasePath
	}
	if strings.TrimSpace(cfg.PostgresDSN) == "" {
		fmt.Fprintln(stderr, "PostgreSQL DSN is required; set CODEX_POOL_POSTGRES_DSN or postgres_dsn")
		return 2
	}
	if !replaceTarget {
		fmt.Fprintln(stderr, "-replace-target is required because the initialized PostgreSQL database contains seed rows")
		return 2
	}
	if timeout <= 0 {
		fmt.Fprintln(stderr, "-timeout must be positive")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fmt.Fprintln(stdout, "maintenance migration started; SQLite writes are frozen until verification completes")
	result, err := storage.MigrateSQLiteToPostgres(ctx, storage.SQLitePostgresMigrationOptions{
		SQLitePath:    sqlitePath,
		PostgresDSN:   cfg.PostgresDSN,
		ReplaceTarget: true,
		Progress: func(table string, rows int64) {
			fmt.Fprintf(stdout, "copied %-40s %d rows\n", table, rows)
		},
	})
	if err != nil {
		message := strings.ReplaceAll(err.Error(), cfg.PostgresDSN, "[redacted PostgreSQL DSN]")
		fmt.Fprintf(stderr, "migration failed; PostgreSQL transaction rolled back and SQLite remains unchanged: %s\n", message)
		return 1
	}
	fmt.Fprintf(stdout, "migration verified and committed: tables=%d rows=%d duration=%s\n", len(result.Tables), result.Rows, result.Duration.Round(time.Millisecond))
	fmt.Fprintln(stdout, "switch storage_driver to postgres and start every node with the same PostgreSQL/Redis configuration")
	return 0
}
