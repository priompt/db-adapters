package priomptdb

import (
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver — single binary, zero external deps
)

// sqlite is the self-hosted default and the catch-all: the DSN is a file path,
// and placeholders are already ?-style, so rebind is the identity.
//
// The pragmas are not optional tuning. The driver defaults to rollback-journal
// mode with a zero busy timeout, which means any write that meets a concurrent
// write returns SQLITE_BUSY immediately rather than waiting — and this store
// sits behind a concurrent gRPC server, so two authors publishing at once is
// ordinary traffic, not a stress case. WAL lets readers run alongside a writer;
// busy_timeout makes writers queue instead of failing. maxConns caps the pool at
// one connection, because SQLite serialises writes anyway and a larger pool only
// converts waiting into lock contention.
var sqlite = dialect{
	driver:   "sqlite",
	match:    func(string) bool { return true },
	rebind:   func(q string) string { return q },
	maxConns: 1,
	dsn:      sqliteDSN,
}

// sqlitePragmas are appended to every SQLite DSN that does not already set them,
// so an operator who wants different values can still override them by hand.
var sqlitePragmas = []struct{ name, clause string }{
	{"busy_timeout", "_pragma=busy_timeout(5000)"},
	{"journal_mode", "_pragma=journal_mode(WAL)"},
	{"synchronous", "_pragma=synchronous(NORMAL)"},
	{"foreign_keys", "_pragma=foreign_keys(1)"},
}

func sqliteDSN(dsn string) string {
	// An in-memory database has no file to journal; leave it alone.
	if strings.HasPrefix(dsn, ":memory:") || strings.Contains(dsn, "mode=memory") {
		return dsn
	}
	var add []string
	for _, p := range sqlitePragmas {
		if !strings.Contains(dsn, p.name) {
			add = append(add, p.clause)
		}
	}
	if len(add) == 0 {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(add, "&")
}
