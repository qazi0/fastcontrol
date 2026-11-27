package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sqliteQueryTimeout         = 5 * time.Second
	sqliteReadOnlyQuery        = "mode=ro&_pragma=busy_timeout(1500)&_pragma=query_only(1)"
	sqliteImmutableQuery       = "immutable=1&_pragma=query_only(1)"
	sqliteErrCantOpen          = 14
	sqliteErrReadonlyRecovery  = 264  // SQLITE_READONLY_RECOVERY
	sqliteErrReadonlyCantInit  = 1288 // SQLITE_READONLY_CANTINIT
	sqliteErrReadonlyDirectory = 1544 // SQLITE_READONLY_DIRECTORY
	sqliteErrIOErrShmOpen      = 4618 // SQLITE_IOERR_SHMOPEN
	sqliteErrIOErrShmSize      = 4874 // SQLITE_IOERR_SHMSIZE
	sqliteErrIOErrShmLock      = 5130 // SQLITE_IOERR_SHMLOCK
	sqliteErrIOErrShmMap       = 5386 // SQLITE_IOERR_SHMMAP
)

func sqliteFileURI(absolute, query string) string {
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	return (&url.URL{Scheme: "file", Path: uriPath}).String() + "?" + query
}

// shouldFallbackImmutable reports whether a mode=ro open or query failed
// because WAL shared-memory files cannot be opened or created (for example,
// inside a read-only directory or sandbox). Explicit write attempts return
// SQLITE_READONLY (8) and do not match.
func shouldFallbackImmutable(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	code := coded.Code()
	if code&0xff == sqliteErrCantOpen {
		return true
	}
	switch code {
	case sqliteErrReadonlyRecovery,
		sqliteErrReadonlyCantInit,
		sqliteErrReadonlyDirectory,
		sqliteErrIOErrShmOpen,
		sqliteErrIOErrShmSize,
		sqliteErrIOErrShmLock,
		sqliteErrIOErrShmMap:
		return true
	}
	return false
}

// withReadOnlySQLite opens path read-only using the pure-Go SQLite driver,
// runs fn within a bounded timeout, and closes the connection before returning
// so no file descriptors or WAL locks linger across polls.
func withReadOnlySQLite(ctx context.Context, path string, fn func(context.Context, *sql.DB) error) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(absolute); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sqliteQueryTimeout)
		defer cancel()
	}

	run := func(query string) error {
		db, err := sql.Open("sqlite", sqliteFileURI(absolute, query))
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		return fn(ctx, db)
	}

	err = run(sqliteReadOnlyQuery)
	if err == nil || ctx.Err() != nil || !shouldFallbackImmutable(err) {
		return err
	}
	return run(sqliteImmutableQuery)
}

func sqliteJSON(path, query string) ([]byte, error) {
	var out []byte
	err := withReadOnlySQLite(context.Background(), path, func(ctx context.Context, db *sql.DB) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		names, err := rows.Columns()
		if err != nil {
			return err
		}
		result := make([]map[string]any, 0, 16)
		values := make([]any, len(names))
		targets := make([]any, len(names))
		for i := range values {
			targets[i] = &values[i]
		}
		for rows.Next() {
			if err := rows.Scan(targets...); err != nil {
				return err
			}
			row := make(map[string]any, len(names))
			for i, name := range names {
				if b, ok := values[i].([]byte); ok {
					row[name] = string(b)
				} else {
					row[name] = values[i]
				}
			}
			result = append(result, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out, err = json.Marshal(result)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
