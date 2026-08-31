// Package priomptdb is the durable, ACID prompt store for Priompt. Backends are
// per-file dialects (sqlite.go, postgres.go — see dialect.go to add one), chosen
// from the DSN at Open. The schema and every query are portable, written once in
// ?-placeholder style; a dialect supplies the driver and the placeholder rewrite.
package priomptdb

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// migrations are schema steps applied in order, once each, tracked in
// schema_version. Append new steps to the end — never edit or reorder an
// existing one, or databases will diverge. Portable across SQLite and Postgres.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS prompts(
		uri          TEXT PRIMARY KEY,
		template     TEXT NOT NULL,
		slots        TEXT NOT NULL,
		version_hash TEXT NOT NULL
	)`,
	// Commit DAG: every version is a commit, content-addressed and pointing at its
	// parent(s). parent2 is set only on merge commits — two parents max, no octopus.
	// Content is snapshotted inline (prompts are tiny; no blob/tree dedup needed).
	`CREATE TABLE IF NOT EXISTS commits(
		hash         TEXT PRIMARY KEY,
		uri          TEXT NOT NULL,
		template     TEXT NOT NULL,
		slots        TEXT NOT NULL,
		version_hash TEXT NOT NULL,
		parent       TEXT,
		parent2      TEXT,
		author       TEXT NOT NULL,
		message      TEXT NOT NULL,
		created_at   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS commits_uri ON commits(uri)`,
	// refs maps a branch name to its tip commit. A branch is just a named pointer.
	`CREATE TABLE IF NOT EXISTS refs(
		uri         TEXT NOT NULL,
		branch      TEXT NOT NULL,
		commit_hash TEXT NOT NULL,
		PRIMARY KEY(uri, branch)
	)`,
}

// DefaultBranch is the trunk a prompt's history accrues on unless told otherwise.
const DefaultBranch = "main"

type Prompt struct {
	URI         string
	Template    string
	Slots       []string
	VersionHash string
}

var ErrNotFound = errors.New("prompt not found")

type Store struct {
	db       *sql.DB
	d        dialect
	version  int
	putQuery string
	getQuery string
	aead     cipher.AEAD // column encryption; nil = plaintext storage
}

// rebind rewrites ?-style placeholders into the backend's native style.
func (s *Store) rebind(q string) string { return s.d.rebind(q) }

// Open connects to SQLite (a file path) or PostgreSQL (a postgres:// DSN) and
// applies any pending schema migrations.
func Open(dsn string) (*Store, error) {
	var d dialect
	for _, c := range dialects {
		if c.match(dsn) {
			d = c
			break
		}
	}
	if d.dsn != nil {
		dsn = d.dsn(dsn)
	}
	db, err := sql.Open(d.driver, dsn)
	if err != nil {
		return nil, err
	}
	if d.maxConns > 0 {
		db.SetMaxOpenConns(d.maxConns)
	}
	version, err := migrate(db, d)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, d: d, version: version}
	// ON CONFLICT upsert is valid in every supported engine; only placeholders differ.
	s.putQuery = d.rebind(`INSERT INTO prompts(uri, template, slots, version_hash) VALUES(?,?,?,?)
		ON CONFLICT(uri) DO UPDATE SET template=excluded.template, slots=excluded.slots, version_hash=excluded.version_hash`)
	s.getQuery = d.rebind(`SELECT uri, template, slots, version_hash FROM prompts WHERE uri=?`)
	if s.aead, err = loadCipher(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion is the number of migrations applied to this database.
func (s *Store) SchemaVersion() int { return s.version }

// migrate applies every migration whose index is past the recorded version,
// each in its own transaction so a failure leaves the schema consistent.
func migrate(db *sql.DB, d dialect) (int, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	var cur int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&cur); err != nil {
		return 0, err
	}
	ph := d.rebind("?")
	for i := cur; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return cur, err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return cur, fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES(`+ph+`)`, i+1); err != nil {
			tx.Rollback()
			return cur, err
		}
		if err := tx.Commit(); err != nil {
			return cur, err
		}
		cur = i + 1
	}
	return cur, nil
}

// Dump returns every stored prompt, ordered by URI — the backup snapshot.
func (s *Store) Dump(ctx context.Context) ([]Prompt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uri, template, slots, version_hash FROM prompts ORDER BY uri`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Prompt
	for rows.Next() {
		var p Prompt
		var slots string
		if err := rows.Scan(&p.URI, &p.Template, &slots, &p.VersionHash); err != nil {
			return nil, err
		}
		if p.Template, err = s.open(p.Template); err != nil {
			return nil, err
		}
		if slots, err = s.open(slots); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(slots), &p.Slots); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// escapeLike neutralizes LIKE metacharacters so a prefix is matched literally.
// A URI segment may legitimately contain '_' (and, in theory, '%'); without this
// they'd act as wildcards. Backslash escapes itself, '%', and '_' under ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// List returns the served-HEAD prompts whose URI starts with prefix, sorted by
// URI — the repo/tree browse. An empty prefix lists the whole store. Only uri and
// version_hash are populated (a listing, not a fetch).
func (s *Store) List(ctx context.Context, prefix string) ([]Prompt, error) {
	q := s.rebind(`SELECT uri, version_hash FROM prompts WHERE uri LIKE ? ESCAPE '\' ORDER BY uri`)
	rows, err := s.db.QueryContext(ctx, q, escapeLike(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Prompt
	for rows.Next() {
		var p Prompt
		if err := rows.Scan(&p.URI, &p.VersionHash); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Hash is the version identity: same content -> same hash -> same cache key.
func Hash(template string, slots []string) string {
	h := sha256.New()
	h.Write([]byte(template))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(slots, "\x00")))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) Put(ctx context.Context, p Prompt) error {
	slots, err := json.Marshal(p.Slots)
	if err != nil {
		return err
	}
	p.VersionHash = Hash(p.Template, p.Slots)
	_, err = s.db.ExecContext(ctx, s.putQuery, p.URI, s.seal(p.Template), s.seal(string(slots)), p.VersionHash)
	return err
}

// Commit is one node in a prompt's history DAG.
type Commit struct {
	Hash        string
	URI         string
	Template    string
	Slots       []string
	VersionHash string
	Parent      string // "" for the root commit
	Parent2     string // set only on merge commits
	Author      string
	Message     string
	CreatedAt   time.Time
}

// CommitHash is the commit identity: which prompt, plus content, plus lineage,
// plus authorship. Two commits with the same content but different parents are
// distinct nodes, which is what lets history branch and reverts not collide
// with the original.
//
// The uri is part of the identity, and must be. Git gets this for free because
// a commit names a tree and a tree encodes path -> blob: identical file content
// shares a *blob* (safe, and deliberate dedup) while the commit still differs
// by path. This store snapshots content inline with no tree layer, so leaving
// the uri out collapsed the two: any two prompts with the same template, author
// and message produced the same hash, the second INSERT hit ON CONFLICT DO
// NOTHING and was dropped, and the surviving row kept the *first* prompt's uri.
// The second prompt's branch then pointed at a commit that disowned it, which
// corrupted its history (across org boundaries) and made rollback impossible.
func CommitHash(uri, versionHash, parent, parent2, author, message string) string {
	h := sha256.New()
	for _, p := range []string{uri, versionHash, parent, parent2, author, message} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var ErrBranchNotFound = errors.New("branch not found")

// tip returns the commit hash a branch points at, or ErrBranchNotFound.
func (s *Store) tip(ctx context.Context, uri, branch string) (string, error) {
	return scanTip(s.db.QueryRowContext(ctx, s.rebind(tipQuery), uri, branch))
}

// tipTx is tip read inside an open transaction, so the compare-and-swap in
// commit can tell a genuine conflict from an idempotent re-commit.
func (s *Store) tipTx(ctx context.Context, tx *sql.Tx, uri, branch string) (string, error) {
	return scanTip(tx.QueryRowContext(ctx, s.rebind(tipQuery), uri, branch))
}

const tipQuery = `SELECT commit_hash FROM refs WHERE uri=? AND branch=?`

func scanTip(row *sql.Row) (string, error) {
	var h string
	err := row.Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBranchNotFound
	}
	return h, err
}

// Commit appends a new version onto branch and advances the branch pointer to it.
// The branch is created (rooted at this commit) if it does not yet exist. Author
// and message are the collaboration metadata: who changed what, and why.
// Returns the new commit hash; committing identical content+meta onto the same
// tip is idempotent (same hash, pointer unchanged).
func (s *Store) Commit(ctx context.Context, uri, branch, template string, slots []string, author, message string) (string, error) {
	parent, err := s.tip(ctx, uri, branch)
	if err != nil && !errors.Is(err, ErrBranchNotFound) {
		return "", err
	}
	return s.commit(ctx, uri, branch, template, slots, parent, "", author, message)
}

// ErrConflict means the branch moved between reading its tip and advancing it —
// someone else committed first. It is retryable: re-read the tip and reapply.
var ErrConflict = errors.New("branch moved: concurrent update")

// ErrAlreadyExists means the named branch is already present on this prompt.
var ErrAlreadyExists = errors.New("branch already exists")

// commit inserts a commit (with explicit parents) and moves branch to it, in one
// transaction. Used by both Commit and Merge.
func (s *Store) commit(ctx context.Context, uri, branch, template string, slots []string, parent, parent2, author, message string) (string, error) {
	slotsJSON, err := json.Marshal(slots)
	if err != nil {
		return "", err
	}
	vh := Hash(template, slots)
	hash := CommitHash(uri, vh, parent, parent2, author, message)
	encT, encSlots := s.seal(template), s.seal(string(slotsJSON))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// INSERT OR IGNORE: re-committing the same node is a no-op, not an error.
	ins := `INSERT INTO commits(hash,uri,template,slots,version_hash,parent,parent2,author,message,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`
	var p, p2 any // NULL, not "", for absent parents
	if parent != "" {
		p = parent
	}
	if parent2 != "" {
		p2 = parent2
	}
	if _, err := tx.ExecContext(ctx, s.rebind(ins), hash, uri, encT, encSlots, vh, p, p2, author, message, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	// Advance the branch as a compare-and-swap against the parent we read, not a
	// blind overwrite. Without the WHERE clause this was a lost update: several
	// writers read the same tip, each built a commit pointing at it, and each
	// overwrote the ref — every call returned success while only the last became
	// reachable. The rest stayed in the commits table as orphans that Log, which
	// walks first-parent, could never reach.
	//
	// This is the same discipline as a Git ref transaction (an update names the
	// old oid and fails if it moved, which is what --force-with-lease exposes),
	// etcd's compare-on-ModRevision, and a conditional write in DynamoDB. The
	// caller gets ErrConflict and retries; nobody is told a write succeeded when
	// it did not.
	var res sql.Result
	if parent == "" && parent2 == "" {
		// Root commit: the branch must not exist yet. If another writer created
		// it first, DO NOTHING leaves 0 rows and we report the conflict.
		root := `INSERT INTO refs(uri,branch,commit_hash) VALUES(?,?,?)
			ON CONFLICT(uri,branch) DO NOTHING`
		res, err = tx.ExecContext(ctx, s.rebind(root), uri, branch, hash)
	} else {
		cas := `UPDATE refs SET commit_hash=? WHERE uri=? AND branch=? AND commit_hash=?`
		res, err = tx.ExecContext(ctx, s.rebind(cas), hash, uri, branch, parent)
	}
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		// Committing the identical node onto the tip it already points at is a
		// no-op, not a conflict — keep Commit idempotent.
		if cur, terr := s.tipTx(ctx, tx, uri, branch); terr == nil && cur == hash {
			return hash, tx.Commit()
		}
		return "", ErrConflict
	}
	// The prompts table is the materialized tip of the trunk — the served HEAD.
	// Keep it in sync whenever a commit (or merge) moves main, in the same tx.
	if branch == DefaultBranch {
		if _, err := tx.ExecContext(ctx, s.putQuery, uri, encT, encSlots, vh); err != nil {
			return "", err
		}
	}
	return hash, tx.Commit()
}

// Branch creates a new branch pointing at the current tip of from. The new branch
// shares from's history until they diverge. Returns that tip's commit hash — the
// caller already needs it and Branch has already resolved it internally, so
// there's no reason to make them look it up again.
func (s *Store) Branch(ctx context.Context, uri, name, from string) (string, error) {
	src, err := s.tip(ctx, uri, from)
	if err != nil {
		return "", err
	}
	// ON CONFLICT DO NOTHING rather than letting the unique index raise: the
	// driver's own message ("UNIQUE constraint failed: refs.uri, refs.branch")
	// used to reach the client verbatim as an INTERNAL error, which is both the
	// wrong code for "already exists" and a disclosure of the schema.
	res, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO refs(uri,branch,commit_hash) VALUES(?,?,?) ON CONFLICT(uri,branch) DO NOTHING`),
		uri, name, src)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrAlreadyExists
	}
	return src, nil
}

// Merge records a merge commit on into, with into's tip as first parent and from's
// tip as second parent, and advances into. Returns the merge commit hash.
//
// ponytail: resolution is "take theirs" — the merge content is from's tip
// verbatim. That records the graph correctly and lets a semantic diff against
// the first parent show exactly what the merge pulled in. Add 3-way content
// auto-merge when divergent edits to the *same* prompt actually need reconciling.
func (s *Store) Merge(ctx context.Context, uri, into, from, author, message string) (string, error) {
	dst, err := s.tip(ctx, uri, into)
	if err != nil {
		return "", err
	}
	srcHash, err := s.tip(ctx, uri, from)
	if err != nil {
		return "", err
	}
	src, err := s.GetCommit(ctx, srcHash)
	if err != nil {
		return "", err
	}
	if message == "" {
		message = fmt.Sprintf("merge %s into %s", from, into)
	}
	return s.commit(ctx, uri, into, src.Template, src.Slots, dst, srcHash, author, message)
}

// Resolve maps a ref to its commit. A ref is a branch name (resolved to its tip)
// or a commit hash. Returns ErrNotFound if it is neither, or names a commit that
// belongs to a different prompt.
func (s *Store) Resolve(ctx context.Context, uri, ref string) (Commit, error) {
	if h, err := s.tip(ctx, uri, ref); err == nil {
		return s.GetCommit(ctx, h)
	} else if !errors.Is(err, ErrBranchNotFound) {
		return Commit{}, err
	}
	c, err := s.GetCommit(ctx, ref)
	if err != nil {
		return Commit{}, err
	}
	if c.URI != uri {
		return Commit{}, ErrNotFound
	}
	return c, nil
}

// SetBranch points branch at an existing commit — rollback (move main back) or
// pinning a branch. On main it re-materializes the served HEAD in the same
// transaction. The commit must belong to uri. Returns the now-tip commit.
func (s *Store) SetBranch(ctx context.Context, uri, branch, commitHash string) (Commit, error) {
	c, err := s.GetCommit(ctx, commitHash)
	if err != nil {
		return Commit{}, err
	}
	if c.URI != uri {
		return Commit{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Commit{}, err
	}
	defer tx.Rollback()

	up := `INSERT INTO refs(uri,branch,commit_hash) VALUES(?,?,?)
		ON CONFLICT(uri,branch) DO UPDATE SET commit_hash=excluded.commit_hash`
	if _, err := tx.ExecContext(ctx, s.rebind(up), uri, branch, commitHash); err != nil {
		return Commit{}, err
	}
	if branch == DefaultBranch {
		slotsJSON, err := json.Marshal(c.Slots)
		if err != nil {
			return Commit{}, err
		}
		if _, err := tx.ExecContext(ctx, s.putQuery, uri, s.seal(c.Template), s.seal(string(slotsJSON)), c.VersionHash); err != nil {
			return Commit{}, err
		}
	}
	return c, tx.Commit()
}

// GetCommit fetches a single commit by hash.
func (s *Store) GetCommit(ctx context.Context, hash string) (Commit, error) {
	return s.scanCommit(s.db.QueryRowContext(ctx, s.rebind(commitCols+` WHERE hash=?`), hash))
}

// Log walks the first-parent chain from a branch tip back to the root — the
// mainline history, newest first. (Like `git log --first-parent`; second parents
// are reachable via GetCommit when you need the full DAG.) Done as one recursive
// query rather than one round-trip per commit: a branch's history only grows,
// and an N-round-trip walk gets slower every time something is published to it.
func (s *Store) Log(ctx context.Context, uri, branch string) ([]Commit, error) {
	h, err := s.tip(ctx, uri, branch)
	if err != nil {
		return nil, err
	}
	const q = `WITH RECURSIVE chain(hash,uri,template,slots,version_hash,parent,parent2,author,message,created_at,depth) AS (
		SELECT hash,uri,template,slots,version_hash,parent,parent2,author,message,created_at,0
			FROM commits WHERE hash=?
		UNION ALL
		SELECT c.hash,c.uri,c.template,c.slots,c.version_hash,c.parent,c.parent2,c.author,c.message,c.created_at,chain.depth+1
			FROM commits c JOIN chain ON c.hash = chain.parent
	)
	SELECT hash,uri,template,slots,version_hash,parent,parent2,author,message,created_at FROM chain ORDER BY depth`
	rows, err := s.db.QueryContext(ctx, s.rebind(q), h)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commit
	for rows.Next() {
		c, err := s.scanCommit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const commitCols = `SELECT hash,uri,template,slots,version_hash,parent,parent2,author,message,created_at FROM commits`

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanCommit(row rowScanner) (Commit, error) {
	var c Commit
	var slots, created string
	var parent, parent2 sql.NullString
	err := row.Scan(&c.Hash, &c.URI, &c.Template, &slots, &c.VersionHash, &parent, &parent2, &c.Author, &c.Message, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Parent, c.Parent2 = parent.String, parent2.String
	if c.Template, err = s.open(c.Template); err != nil {
		return c, err
	}
	if slots, err = s.open(slots); err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(slots), &c.Slots); err != nil {
		return c, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return c, nil
}

func (s *Store) Get(ctx context.Context, uri string) (Prompt, error) {
	var p Prompt
	var slots string
	err := s.db.QueryRowContext(ctx, s.getQuery, uri).
		Scan(&p.URI, &p.Template, &slots, &p.VersionHash)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if p.Template, err = s.open(p.Template); err != nil {
		return p, err
	}
	if slots, err = s.open(slots); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(slots), &p.Slots); err != nil {
		return p, err
	}
	return p, nil
}

// --- whole-store dump and load (backup / restore) --------------------------
//
// Dump above returns the served HEAD only — the prompts table. That is the
// right shape for exporting content, and the wrong shape for a backup: a
// restore built from it produces a database that serves correctly and has no
// history at all, no branches, and no commit for main to point at. Every
// version-control operation the product exists for is silently gone, while the
// prompts still resolve, so the result looks healthy.
//
// These carry the other two tables. The split mirrors the one every comparable
// system draws — Dolt separates `dump` from `backup`, Postgres separates
// pg_dump from pg_basebackup plus WAL — because a logical export of current
// state genuinely cannot reconstruct history, and pretending otherwise is how
// people lose it.

// Ref is one branch pointer: which commit a branch of a prompt points at.
type Ref struct {
	URI    string
	Branch string
	Commit string
}

// DumpCommits returns every commit in the store, oldest first so that a restore
// can insert them in an order where each commit's parents already exist.
func (s *Store) DumpCommits(ctx context.Context) ([]Commit, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(commitCols+` ORDER BY created_at, hash`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commit
	for rows.Next() {
		c, err := s.scanCommit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DumpRefs returns every branch pointer in the store.
func (s *Store) DumpRefs(ctx context.Context) ([]Ref, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uri, branch, commit_hash FROM refs ORDER BY uri, branch`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.URI, &r.Branch, &r.Commit); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutCommit inserts a commit verbatim, preserving its hash, parents, author,
// message and timestamp. Restoring must not recompute the hash: the hash is the
// commit's identity, every child names it as a parent, and recomputing would
// break the chain. Re-inserting an existing commit is a no-op.
func (s *Store) PutCommit(ctx context.Context, c Commit) error {
	slotsJSON, err := json.Marshal(c.Slots)
	if err != nil {
		return err
	}
	var p, p2 any // NULL, not "", for absent parents
	if c.Parent != "" {
		p = c.Parent
	}
	if c.Parent2 != "" {
		p2 = c.Parent2
	}
	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	q := `INSERT INTO commits(hash,uri,template,slots,version_hash,parent,parent2,author,message,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`
	_, err = s.db.ExecContext(ctx, s.rebind(q),
		c.Hash, c.URI, s.seal(c.Template), s.seal(string(slotsJSON)), c.VersionHash,
		p, p2, c.Author, c.Message, created.UTC().Format(time.RFC3339Nano))
	return err
}

// PutRef points a branch at a commit, creating or moving it. Unlike SetBranch
// this does not re-materialize the served HEAD or check that the commit belongs
// to the prompt — a restore replays recorded state rather than performing a
// rollback, and the prompts table is restored from its own records.
func (s *Store) PutRef(ctx context.Context, r Ref) error {
	q := `INSERT INTO refs(uri,branch,commit_hash) VALUES(?,?,?)
		ON CONFLICT(uri,branch) DO UPDATE SET commit_hash=excluded.commit_hash`
	_, err := s.db.ExecContext(ctx, s.rebind(q), r.URI, r.Branch, r.Commit)
	return err
}

// CountCommits reports how many commits the store holds. Restore uses it to
// refuse to write over a database that already has history.
func (s *Store) CountCommits(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commits`).Scan(&n)
	return n, err
}
