package main

func sqliteJSON(db, query string) ([]byte, error) { return sqliteJSONCLI(db, query) }
