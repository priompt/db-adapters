package priomptdb

import (
	"context"
	"path/filepath"
	"testing"
)

// A backup is only a backup if restoring it gives you back what you had. The
// served-HEAD dump cannot: it has no commits and no refs, so the restored store
// serves content and has lost every version-control operation the product
// exists for — while still looking healthy, because the prompts resolve.
func TestFullRoundTripPreservesHistory(t *testing.T) {
	ctx := context.Background()
	src, err := Open(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	const uri = "priompt://acme/support/agent"
	var hashes []string
	for _, v := range []string{"v1 for {org}.", "v2 for {org}.", "v3 for {org}."} {
		h, err := src.Commit(ctx, uri, DefaultBranch, v, []string{"org"}, "ann", "msg "+v)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	if _, err := src.Branch(ctx, uri, "dev", DefaultBranch); err != nil {
		t.Fatal(err)
	}

	// --- dump everything ---
	prompts, err := src.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := src.DumpCommits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := src.DumpRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 {
		t.Fatalf("dumped %d commits, want 3", len(commits))
	}
	if len(refs) != 2 {
		t.Fatalf("dumped %d refs, want 2 (main + dev)", len(refs))
	}

	// --- load into a fresh store, commits before refs ---
	dst, err := Open(filepath.Join(t.TempDir(), "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	for _, c := range commits {
		if err := dst.PutCommit(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range refs {
		if err := dst.PutRef(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range prompts {
		if err := dst.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	// --- everything that was there must still be there ---
	log, err := dst.Log(ctx, uri, DefaultBranch)
	if err != nil {
		t.Fatalf("history lost: %v", err)
	}
	if len(log) != 3 {
		t.Fatalf("restored history has %d commits, want 3", len(log))
	}
	// Commit identity must survive verbatim — children name their parents by
	// hash, so a recomputed hash breaks the chain.
	for i, want := range []string{hashes[2], hashes[1], hashes[0]} {
		if log[i].Hash != want {
			t.Errorf("commit %d hash = %s, want %s", i, log[i].Hash[:12], want[:12])
		}
	}
	if log[0].Author != "ann" || log[0].Message == "" {
		t.Errorf("authorship lost: author=%q message=%q", log[0].Author, log[0].Message)
	}
	if _, err := dst.Resolve(ctx, uri, "dev"); err != nil {
		t.Errorf("dev branch lost: %v", err)
	}
	// And rollback — the operation the history exists for — must work.
	if _, err := dst.SetBranch(ctx, uri, DefaultBranch, hashes[0]); err != nil {
		t.Errorf("rollback after restore failed: %v", err)
	}
	p, err := dst.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if p.Template != "v1 for {org}." {
		t.Errorf("after rollback serving %q, want v1", p.Template)
	}
}

// Restoring the same backup twice must not duplicate or corrupt anything.
func TestFullRestoreIsIdempotent(t *testing.T) {
	ctx := context.Background()
	src, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer src.Close()
	const uri = "priompt://acme/x/y"
	src.Commit(ctx, uri, DefaultBranch, "one {org}.", []string{"org"}, "a", "m1")
	src.Commit(ctx, uri, DefaultBranch, "two {org}.", []string{"org"}, "a", "m2")

	commits, _ := src.DumpCommits(ctx)
	refs, _ := src.DumpRefs(ctx)

	dst, _ := Open(filepath.Join(t.TempDir(), "d.db"))
	defer dst.Close()
	for pass := 0; pass < 2; pass++ {
		for _, c := range commits {
			if err := dst.PutCommit(ctx, c); err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
		}
		for _, r := range refs {
			if err := dst.PutRef(ctx, r); err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
		}
	}
	n, err := dst.CountCommits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("after restoring twice the store holds %d commits, want 2", n)
	}
}

// CountCommits is what lets restore refuse to write over existing history.
func TestCountCommitsSeesHistory(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "c.db"))
	defer st.Close()
	if n, _ := st.CountCommits(ctx); n != 0 {
		t.Fatalf("fresh store reports %d commits", n)
	}
	st.Commit(ctx, "priompt://acme/a/b", DefaultBranch, "x {org}.", []string{"org"}, "a", "m")
	if n, _ := st.CountCommits(ctx); n != 1 {
		t.Fatalf("after one commit reports %d", n)
	}
}
