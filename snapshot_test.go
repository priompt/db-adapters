package priomptdb

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// dumpFn is how a backup reads the store: Snapshot, or the separate dumps.
type dumpFn func(ctx context.Context, s *Store) ([]Prompt, []Commit, []Ref, error)

func snapshotDump(ctx context.Context, s *Store) ([]Prompt, []Commit, []Ref, error) {
	return s.Snapshot(ctx)
}

// consistent reports why a dump could not be restored as-is: a ref naming a
// commit the dump lacks, or a served prompt that is not main's commit.
func consistent(prompts []Prompt, commits []Commit, refs []Ref) error {
	byHash := map[string]Commit{}
	for _, c := range commits {
		byHash[c.Hash] = c
	}
	mainVersion := map[string]string{}
	for _, r := range refs {
		c, ok := byHash[r.Commit]
		if !ok {
			return fmt.Errorf("ref %s@%s names commit %s, which is not in the dump", r.URI, r.Branch, r.Commit)
		}
		if r.Branch == DefaultBranch {
			mainVersion[r.URI] = c.VersionHash
		}
	}
	for _, p := range prompts {
		if v := mainVersion[p.URI]; v != p.VersionHash {
			return fmt.Errorf("served %s is %s but main's commit has %s", p.URI, p.VersionHash, v)
		}
	}
	return nil
}

// A backup taken while another process publishes must still be one moment in
// time. The writer and the reader use separate Store handles (separate pools),
// like `priompt serve` and `priompt backup` sharing a database file.
func TestSnapshotIsConsistentUnderConcurrentWrites(t *testing.T) {
	checkDumpUnderWrites(t, snapshotDump, 200)
}

func checkDumpUnderWrites(t *testing.T, dump dumpFn, rounds int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snap.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()
	const uri = "priompt://acme/snap/agent"
	if _, err := writer.Commit(ctx, uri, DefaultBranch, "v0", nil, "t", "seed"); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; !stop.Load(); i++ {
			writer.Commit(ctx, uri, DefaultBranch, fmt.Sprintf("v%d", i), nil, "t", "edit")
		}
	}()
	defer func() { stop.Store(true); <-done }()

	for i := 0; i < rounds; i++ {
		prompts, commits, refs, err := dump(ctx, reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := consistent(prompts, commits, refs); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
}
