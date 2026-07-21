# priompt db-adapters

Durable, ACID prompt store for **Priompt** — prompts, the commit DAG, branches,
and refs — with optional AES-256-GCM encryption at rest
(`PRIOMPT_ENCRYPTION_KEY`). Backends: **SQLite** (pure-Go driver, the
self-hosted default) and **PostgreSQL** (multi-node).

```go
import store "priomptdb"

st, _ := store.Open("prompts.db")                  // SQLite file path
st, _ := store.Open("postgres://user:pw@host/db")  // PostgreSQL DSN
```

## Adding a database

All SQL is portable and written once in `?`-placeholder style; a backend only
supplies a `database/sql` driver name, a DSN matcher, and the placeholder
rewrite (see `dialect.go`).

1. Create `<name>.go` with a `dialect` var, blank-importing its driver —
   `postgres.go` is the pattern.
2. Add it to `dialects` in `dialect.go`, before `sqlite` (the catch-all).
3. The queries use `ON CONFLICT` upserts — engines without them (e.g. MySQL's
   `ON DUPLICATE KEY`) need a per-dialect upsert clause; add it to `dialect`
   when that backend actually lands.

## Relation to the core repo

The Priompt server currently carries its own copy of this package
(`internal/store`) so it stays buildable standalone; this repo is the
importable library version. Sync changes between the two until core switches
to importing this module.
