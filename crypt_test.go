package priomptdb

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptionRoundTrip(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32)) // all-zero 32-byte key
	t.Setenv("PRIOMPT_ENCRYPTION_KEY", key)

	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "enc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	const uri, tmpl = "priompt://acme/x", "secret {name}"
	if _, err := st.Commit(ctx, uri, DefaultBranch, tmpl, []string{"name"}, "me", "msg"); err != nil {
		t.Fatal(err)
	}

	// Read back through the store -> plaintext.
	got, err := st.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if got.Template != tmpl || len(got.Slots) != 1 || got.Slots[0] != "name" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Raw column bytes must be ciphertext, not the plaintext template.
	var rawT, rawS string
	if err := st.db.QueryRow(`SELECT template, slots FROM prompts WHERE uri=?`, uri).Scan(&rawT, &rawS); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rawT, encPrefixBound) || strings.Contains(rawT, "secret") {
		t.Errorf("template stored in the clear: %q", rawT)
	}
	if !strings.HasPrefix(rawS, encPrefixBound) || strings.Contains(rawS, "name") {
		t.Errorf("slots stored in the clear: %q", rawS)
	}
}

func TestPlaintextRowReadableWithKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mix.db")

	// Write a row with NO key configured (plaintext, no marker).
	plain, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const uri = "priompt://acme/legacy"
	if _, err := plain.Commit(ctx, uri, DefaultBranch, "old {x}", []string{"x"}, "me", "m"); err != nil {
		t.Fatal(err)
	}
	plain.Close()

	// Reopen WITH a key: the unmarked legacy row must still read back.
	t.Setenv("PRIOMPT_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	enc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	got, err := enc.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if got.Template != "old {x}" {
		t.Fatalf("legacy plaintext row unreadable: %+v", got)
	}
}

// A row written with a key, then read by a process without one, used to come
// back as the stored ciphertext: Get returned "enc1:..." as the template and
// the slots decode failed with a JSON syntax error that said nothing about the
// missing key. Every read path now says what is wrong instead.
func TestEncryptedRowWithoutKeyIsAClearError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "enc.db")
	ctx := context.Background()
	const uri = "priompt://acme/support/tier2/refunds"

	t.Setenv("PRIOMPT_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	enc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Commit(ctx, uri, DefaultBranch, "secret {name}", []string{"name"}, "me", "msg"); err != nil {
		t.Fatal(err)
	}
	enc.Close()

	t.Setenv("PRIOMPT_ENCRYPTION_KEY", "")
	noKey, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer noKey.Close()

	t.Run("Get", func(t *testing.T) {
		if _, err := noKey.Get(ctx, uri); !errors.Is(err, ErrEncryptionKeyMissing) {
			t.Fatalf("Get: %v, want ErrEncryptionKeyMissing", err)
		}
	})
	t.Run("Resolve", func(t *testing.T) {
		if _, err := noKey.Resolve(ctx, uri, DefaultBranch); !errors.Is(err, ErrEncryptionKeyMissing) {
			t.Fatalf("Resolve: %v, want ErrEncryptionKeyMissing", err)
		}
	})
	t.Run("Dump", func(t *testing.T) {
		if _, err := noKey.Dump(ctx); !errors.Is(err, ErrEncryptionKeyMissing) {
			t.Fatalf("Dump: %v, want ErrEncryptionKeyMissing", err)
		}
	})
}

// The marker alone decides, so a plaintext row keeps reading back in a mixed
// database even though other rows are encrypted.
func TestEncryptedAndPlaintextRowsCoexistWithoutAKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mixed.db")
	ctx := context.Background()

	plain, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Commit(ctx, "priompt://acme/plain", DefaultBranch, "plain {x}", []string{"x"}, "me", "m"); err != nil {
		t.Fatal(err)
	}
	plain.Close()

	t.Setenv("PRIOMPT_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	enc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Commit(ctx, "priompt://acme/secret", DefaultBranch, "secret {x}", []string{"x"}, "me", "m"); err != nil {
		t.Fatal(err)
	}
	enc.Close()

	t.Setenv("PRIOMPT_ENCRYPTION_KEY", "")
	noKey, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer noKey.Close()

	if got, err := noKey.Get(ctx, "priompt://acme/plain"); err != nil || got.Template != "plain {x}" {
		t.Fatalf("plaintext row: %+v %v", got, err)
	}
	if _, err := noKey.Get(ctx, "priompt://acme/secret"); !errors.Is(err, ErrEncryptionKeyMissing) {
		t.Fatalf("encrypted row: %v, want ErrEncryptionKeyMissing", err)
	}
}
