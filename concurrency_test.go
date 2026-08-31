package priomptdb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Two prompts with identical content, author and message must not collide. The
// uri is part of the commit identity; without it the second INSERT hit
// ON CONFLICT DO NOTHING, the surviving row kept the first prompt's uri, and the
// second prompt's history was silently attributed to the first — across orgs.
func TestCommitIdentityIncludesURI(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	const tmpl = "Escalate every {org} request to a human."
	slots := []string{"org"}
	aURI, bURI := "priompt://acme/support/agent", "priompt://beta/support/agent"

	aHash, err := st.Commit(ctx, aURI, DefaultBranch, tmpl, slots, "alice", "initial")
	if err != nil {
		t.Fatal(err)
	}
	bHash, err := st.Commit(ctx, bURI, DefaultBranch, tmpl, slots, "alice", "initial")
	if err != nil {
		t.Fatal(err)
	}
	if aHash == bHash {
		t.Fatalf("two prompts share commit hash %s", aHash[:16])
	}

	// beta's history must contain only beta's commits.
	log, err := st.Log(ctx, bURI, DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range log {
		if c.URI != bURI {
			t.Errorf("history of %s contains a commit owned by %s", bURI, c.URI)
		}
	}

	// And rollback to a commit History reported must work.
	if _, err := st.SetBranch(ctx, bURI, DefaultBranch, bHash); err != nil {
		t.Errorf("rollback to own history commit failed: %v", err)
	}
}

// Concurrent writers must never all be told they succeeded. Exactly one commit
// wins each round; the losers get ErrConflict and can retry.
func TestConcurrentCommitsDoNotLose(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const uri = "priompt://acme/concur/agent"

	if _, err := st.Commit(ctx, uri, DefaultBranch, "seed for {org}.", []string{"org"}, "seed", "seed"); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, conflict int
	accepted := map[string]bool{}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("write-%02d", i)
			_, err := st.Commit(ctx, uri, DefaultBranch,
				fmt.Sprintf("Concurrent writer %02d for {org}.", i), []string{"org"}, "author", msg)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				accepted[msg] = true
			case errors.Is(err, ErrConflict):
				conflict++
			default:
				t.Errorf("unexpected error from writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if ok+conflict != writers {
		t.Fatalf("accounted %d of %d writers", ok+conflict, writers)
	}
	if ok == 0 {
		t.Fatal("every writer conflicted; at least one must win")
	}
	t.Logf("%d succeeded, %d conflicted (retryable)", ok, conflict)

	// The contract: everything reported as succeeded is reachable in history.
	log, err := st.Log(ctx, uri, DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	reachable := map[string]bool{}
	for _, c := range log {
		reachable[c.Message] = true
	}
	for msg := range accepted {
		if !reachable[msg] {
			t.Errorf("commit %q was acknowledged but is not reachable in history", msg)
		}
	}
}

// Retrying on ErrConflict must converge: every writer eventually lands.
func TestConcurrentCommitsConvergeWithRetry(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const uri = "priompt://acme/retry/agent"

	if _, err := st.Commit(ctx, uri, DefaultBranch, "seed for {org}.", []string{"org"}, "seed", "seed"); err != nil {
		t.Fatal(err)
	}

	const writers = 12
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for attempt := 0; attempt < 100; attempt++ {
				_, err := st.Commit(ctx, uri, DefaultBranch,
					fmt.Sprintf("Retrying writer %02d for {org}.", i), []string{"org"},
					"author", fmt.Sprintf("retry-%02d", i))
				if err == nil {
					return
				}
				if !errors.Is(err, ErrConflict) {
					t.Errorf("writer %d: %v", i, err)
					return
				}
			}
			t.Errorf("writer %d never landed", i)
		}(i)
	}
	wg.Wait()

	log, err := st.Log(ctx, uri, DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != writers+1 {
		t.Errorf("history has %d commits, want %d (seed + %d writers)", len(log), writers+1, writers)
	}
}

// Replaying the identical commit node — same parent, content, author, message —
// must be a no-op that reports success, not a spurious conflict. This is what
// makes publish safely retryable: a client that retries after a timeout, or two
// writers that raced from the same tip, both produce this exact hash, and the
// second must not be told the branch moved when it is already exactly where it
// wanted to put it.
func TestReplayingSameCommitNodeIsIdempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const uri = "priompt://acme/idem/agent"

	seed, err := st.Commit(ctx, uri, DefaultBranch, "seed {org}.", []string{"org"}, "ann", "seed")
	if err != nil {
		t.Fatal(err)
	}

	// Both calls name the same parent explicitly, so both compute one hash.
	h1, err := st.commit(ctx, uri, DefaultBranch, "same {org}.", []string{"org"}, seed, "", "ann", "once")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := st.commit(ctx, uri, DefaultBranch, "same {org}.", []string{"org"}, seed, "", "ann", "once")
	if err != nil {
		t.Fatalf("replaying the same commit node: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("replay changed hash: %s -> %s", h1[:12], h2[:12])
	}
	if log, _ := st.Log(ctx, uri, DefaultBranch); len(log) != 2 {
		t.Fatalf("history has %d commits, want 2 (seed + the one node)", len(log))
	}
}

// A writer whose parent is stale — the branch moved on after it read the tip —
// must get ErrConflict rather than silently overwriting the newer commit.
func TestStaleParentConflicts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const uri = "priompt://acme/stale/agent"

	stale, err := st.Commit(ctx, uri, DefaultBranch, "v1 {org}.", []string{"org"}, "ann", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Commit(ctx, uri, DefaultBranch, "v2 {org}.", []string{"org"}, "bob", "v2"); err != nil {
		t.Fatal(err)
	}
	// Now commit as if we had only ever seen v1.
	_, err = st.commit(ctx, uri, DefaultBranch, "v3 {org}.", []string{"org"}, stale, "", "carol", "v3")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-parent commit = %v, want ErrConflict", err)
	}
	// v2 must still be the served HEAD and the tip.
	p, err := st.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if p.Template != "v2 {org}." {
		t.Fatalf("served HEAD = %q, want v2 — the conflicting write overwrote it", p.Template)
	}
}

func TestBranchAlreadyExists(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const uri = "priompt://acme/br/agent"

	if _, err := st.Commit(ctx, uri, DefaultBranch, "x {org}.", []string{"org"}, "a", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Branch(ctx, uri, "feature", DefaultBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Branch(ctx, uri, "feature", DefaultBranch); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate branch = %v, want ErrAlreadyExists", err)
	}
}
