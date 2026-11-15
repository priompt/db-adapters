package priomptdb

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"testing"
)

func encStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("PRIOMPT_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	st, err := Open(filepath.Join(t.TempDir(), "bind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Copying one prompt's encrypted columns into another row must not make the
// second URI serve the first prompt's content.
func TestCiphertextMovedToAnotherPromptRowFails(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	if _, err := st.Commit(ctx, "priompt://acme/a", DefaultBranch, "secret {x}", []string{"x"}, "t", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Commit(ctx, "priompt://evil/b", DefaultBranch, "mine {y}", []string{"y"}, "t", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE prompts SET
		template=(SELECT template FROM prompts WHERE uri='priompt://acme/a'),
		slots=(SELECT slots FROM prompts WHERE uri='priompt://acme/a'),
		version_hash=(SELECT version_hash FROM prompts WHERE uri='priompt://acme/a')
		WHERE uri='priompt://evil/b'`); err != nil {
		t.Fatal(err)
	}
	if p, err := st.Get(ctx, "priompt://evil/b"); err == nil {
		t.Fatalf("swapped ciphertext decrypted: served %q at priompt://evil/b", p.Template)
	}
	if _, err := st.Get(ctx, "priompt://acme/a"); err != nil {
		t.Fatalf("untouched row: %v", err)
	}
}

// The same holds for commit rows, which history and rollback read.
func TestCiphertextMovedToAnotherCommitRowFails(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	ha, err := st.Commit(ctx, "priompt://acme/a", DefaultBranch, "secret {x}", []string{"x"}, "t", "m")
	if err != nil {
		t.Fatal(err)
	}
	hb, err := st.Commit(ctx, "priompt://evil/b", DefaultBranch, "mine {y}", []string{"y"}, "t", "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(st.rebind(`UPDATE commits SET
		template=(SELECT template FROM commits WHERE hash=?),
		slots=(SELECT slots FROM commits WHERE hash=?) WHERE hash=?`), ha, ha, hb); err != nil {
		t.Fatal(err)
	}
	if c, err := st.GetCommit(ctx, hb); err == nil {
		t.Fatalf("swapped commit ciphertext decrypted: %q", c.Template)
	}
}

// Rows sealed before row binding (enc1, no associated data) stay readable.
func TestLegacyUnboundCiphertextStillReadable(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	legacy := func(plain string) string {
		nonce := make([]byte, st.aead.NonceSize())
		rand.Read(nonce)
		return encPrefix + base64.StdEncoding.EncodeToString(st.aead.Seal(nonce, nonce, []byte(plain), nil))
	}
	if _, err := st.db.Exec(st.putQuery, "priompt://acme/old", legacy("old {x}"), legacy(`["x"]`), Hash("old {x}", []string{"x"})); err != nil {
		t.Fatal(err)
	}
	p, err := st.Get(ctx, "priompt://acme/old")
	if err != nil || p.Template != "old {x}" || len(p.Slots) != 1 {
		t.Fatalf("legacy enc1 row: %+v, %v", p, err)
	}
}
