package priomptdb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// encPrefix marks an encrypted column value. Plaintext values (written before a
// key was configured) lack it and are returned as-is, so encrypted and plaintext
// rows coexist during rollout — no migration needed to turn encryption on.
//
// ponytail: a plaintext template literally beginning with "enc1:", written while
// encryption was OFF and later read with a key ON, would be mis-detected as
// ciphertext and fail to decrypt. Vanishingly unlikely for prompt text; move the
// marker to its own column if it ever bites.
const encPrefix = "enc1:"

// encPrefixBound marks a column value sealed with associated data naming its
// row and column (see promptAAD, commitAAD). AES-GCM authenticates the
// associated data, so a ciphertext copied into another row or column fails to
// decrypt instead of serving another prompt's content. New writes always use
// it; enc1 values (no associated data) stay readable so existing databases
// keep working, and a backup + restore re-seals them in this format.
const encPrefixBound = "enc2:"

// promptAAD and commitAAD bind a sealed column to its row: a prompts row by
// URI, a commits row by commit hash (which itself covers URI and content).
func promptAAD(uri, column string) string   { return "prompts\x00" + uri + "\x00" + column }
func commitAAD(hash, column string) string { return "commits\x00" + hash + "\x00" + column }

// A Sealer encrypts and decrypts values with the configured key. It is exported
// because the database is not the only place prompt content comes to rest: the
// server's shared Redis cache holds the same templates, and an "encrypted at
// rest" guarantee that stops at one of two persistent stores is not a guarantee.
// One implementation of the crypto, used by both.
type Sealer struct{ aead cipher.AEAD }

// NewSealer builds a Sealer from PRIOMPT_ENCRYPTION_KEY. A nil-aead Sealer (no
// key configured) is valid and passes values through unchanged, so callers need
// no branch of their own.
func NewSealer() (*Sealer, error) {
	aead, err := loadCipher()
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Enabled reports whether a key is configured.
func (s *Sealer) Enabled() bool { return s != nil && s.aead != nil }

// Seal encrypts a value; passthrough when no key is configured.
func (s *Sealer) Seal(plain string) string {
	if !s.Enabled() {
		return plain
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	ct := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct)
}

// SealBytes is Seal for opaque binary payloads — a marshalled protobuf, say.
func (s *Sealer) SealBytes(plain []byte) []byte {
	if !s.Enabled() {
		return plain
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return append([]byte(encPrefix), s.aead.Seal(nonce, nonce, plain, nil)...)
}

// OpenBytes reverses SealBytes. A value without the marker is returned as-is.
func (s *Sealer) OpenBytes(stored []byte) ([]byte, error) {
	if !s.Enabled() || !bytes.HasPrefix(stored, []byte(encPrefix)) {
		return stored, nil
	}
	ct := stored[len(encPrefix):]
	n := s.aead.NonceSize()
	if len(ct) < n {
		return nil, errors.New("encrypted value too short")
	}
	return s.aead.Open(nil, ct[:n], ct[n:], nil)
}

// loadCipher builds the column cipher from PRIOMPT_ENCRYPTION_KEY (base64 of a
// 32-byte key) for AES-256-GCM. Unset -> nil (plaintext storage, unchanged).
// Set but invalid -> error: if the operator asked for encryption, never silently
// fall back to storing plaintext.
func loadCipher() (cipher.AEAD, error) {
	raw := os.Getenv("PRIOMPT_ENCRYPTION_KEY")
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("PRIOMPT_ENCRYPTION_KEY: not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("PRIOMPT_ENCRYPTION_KEY: want 32 bytes base64-encoded, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts a column value bound to aad (its row and column) — a random
// nonce is prepended to the ciphertext, the whole thing base64'd and marked.
// Passthrough when no key is configured. Non-deterministic (fresh nonce each
// call), which is fine: identity and dedup key off version_hash, computed over
// plaintext, never the stored column.
func (s *Store) seal(plain, aad string) string {
	if s.aead == nil {
		return plain
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	ct := s.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return encPrefixBound + base64.StdEncoding.EncodeToString(ct)
}

// open reverses seal; aad must name the row and column the value was read
// from. A value without a marker is plaintext (a pre-key row, or no key
// configured) and returned unchanged; an enc1 value predates row binding and
// is opened without associated data.
func (s *Store) open(stored, aad string) (string, error) {
	if s.aead == nil {
		return stored, nil
	}
	var body string
	var ad []byte
	switch {
	case strings.HasPrefix(stored, encPrefixBound):
		body, ad = stored[len(encPrefixBound):], []byte(aad)
	case strings.HasPrefix(stored, encPrefix):
		body = stored[len(encPrefix):]
	default:
		return stored, nil
	}
	ct, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", err
	}
	n := s.aead.NonceSize()
	if len(ct) < n {
		return "", errors.New("encrypted column too short")
	}
	plain, err := s.aead.Open(nil, ct[:n], ct[n:], ad)
	if err != nil {
		return "", fmt.Errorf("decrypt column: %w", err)
	}
	return string(plain), nil
}
