package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func sqliteJSON(path, query string) ([]byte, error) {
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&_pragma=busy_timeout(500)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(names))
		targets := make([]any, len(names))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := map[string]any{}
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
		return nil, err
	}
	return json.Marshal(result)
}
