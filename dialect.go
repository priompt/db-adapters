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
}

// dialects is probed in order at Open. Keep sqlite last: any DSN no other
// backend claims is treated as a SQLite file path.
var dialects = []dialect{postgres, sqlite}
