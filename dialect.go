package priomptdb

// A dialect is everything one database backend must provide. The schema and all
// queries are portable, written once in ?-placeholder style; a backend supplies
// its database/sql driver name, a DSN matcher, and the placeholder rewrite.
//
// To add a database: create <name>.go with a dialect var (blank-importing its
// driver — postgres.go is the pattern), then list it in dialects below.
//
// ponytail: dialect covers driver+DSN+placeholders only; engines without
// ON CONFLICT upserts (MySQL) will need a per-dialect upsert clause here when
// one actually arrives.
type dialect struct {
	driver string                // database/sql driver name
	match  func(dsn string) bool // does this DSN belong to this backend?
	rebind func(q string) string // ?-style placeholders -> native style

	// dsn optionally rewrites the DSN before it reaches the driver — where a
	// backend needs connection settings that are correctness, not tuning
	// (SQLite's WAL and busy timeout). nil means "use the DSN as given".
	dsn func(dsn string) string
	// maxConns caps the connection pool. 0 means unlimited (the database/sql
	// default); SQLite sets 1 because it serialises writes regardless.
	maxConns int
}

// dialects is probed in order at Open. Keep sqlite last: any DSN no other
// backend claims is treated as a SQLite file path.
var dialects = []dialect{postgres, sqlite}
