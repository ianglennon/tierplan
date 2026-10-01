// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package store owns Bayplan's SQLite database: the connection pools, the
// embedded goose migrations that run at startup, and the generated sqlc
// queries (D-82 to D-86).
//
// Conventions every table follows (RESEARCH Pitfall 16):
//   - timestamps are INTEGER, UTC unix nanoseconds;
//   - IDs are UUIDv7 stored as TEXT;
//   - there are no down-migrations in 0.x (D-84).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync/atomic"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// DatabaseFile is the database's file name inside the data directory.
const DatabaseFile = "bayplan.db"

const (
	// pragmas applied to every connection of both pools. The order matters
	// only for readability; modernc runs them in sequence on each new
	// connection.
	commonPragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	readPoolSize = 4
)

// Store holds the two connection pools and the queries bound to each.
//
// SQLite has one writer at a time, so all writes go through Write, a pool of
// exactly one connection that starts every transaction with BEGIN IMMEDIATE
// (RESEARCH Pitfall 15). Reads use the separate Read pool, which is opened
// query_only so a stray write through it fails loudly instead of contending.
type Store struct {
	Write, Read *sql.DB
	// Q runs on the write pool, RQ on the read pool.
	Q, RQ *sqlc.Queries
	// Path is the database file.
	Path string

	// schemaVersion is the version Migrate brought the database to; zero until
	// a migration run has succeeded.
	schemaVersion atomic.Int64
}

// Open opens <dataDir>/bayplan.db with WAL, a 5 second busy timeout and
// foreign keys on. It does not migrate: call Migrate before using the store.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	abs, err := filepath.Abs(filepath.Join(dataDir, DatabaseFile))
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}

	write, err := openPool(abs, commonPragmas+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database for writing: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	// The WAL header is created by the first write-pool connection, so the
	// read pool is opened only after that connection exists.
	if err := write.PingContext(ctx); err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open database for writing: %w", err)
	}

	read, err := openPool(abs, commonPragmas+"&_pragma=query_only(1)")
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open database for reading: %w", err)
	}
	read.SetMaxOpenConns(readPoolSize)
	read.SetMaxIdleConns(readPoolSize)
	if err := read.PingContext(ctx); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("open database for reading: %w", err)
	}

	return &Store{
		Write: write, Read: read,
		Q: sqlc.New(write), RQ: sqlc.New(read),
		Path: abs,
	}, nil
}

func openPool(absPath, query string) (*sql.DB, error) {
	// url.URL escapes characters in the path that SQLite's URI parser treats
	// as syntax (?, #, %), so an unusual data directory name still works.
	u := url.URL{Scheme: "file", Path: absPath, RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Close closes both pools. It is safe to call more than once.
func (s *Store) Close() error {
	return errors.Join(s.Read.Close(), s.Write.Close())
}

// Ready reports whether the database is open and at this binary's target
// schema version. It backs /readyz (D-86). The schema check is cached from the
// successful Migrate call; the connection check is live.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.Read.PingContext(ctx); err != nil {
		return fmt.Errorf("database is not reachable: %w", err)
	}
	target, err := embeddedTarget()
	if err != nil {
		return err
	}
	if got := s.schemaVersion.Load(); got != target {
		return fmt.Errorf("database schema is at version %d, this binary needs %d", got, target)
	}
	return nil
}
