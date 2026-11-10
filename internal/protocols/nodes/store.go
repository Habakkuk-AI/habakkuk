package nodes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/core/store"
)

// Migrations returns nodes.db's schema steps, for version reports and
// upgrade checks.
func Migrations() []store.Migration { return migrations }

// migrations for nodes.db: paired nodes (with a hash of each one's secret,

// never the secret) and pairing codes (hashed too, single use). It is its
// own file so control.db never changes for nodes, and `hk node pair`
// and `hk nodes revoke` can write it while `hk serve` runs.
var migrations = []store.Migration{{Version: 1, SQL: `
CREATE TABLE nodes (
	name        TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL,
	platform    TEXT NOT NULL DEFAULT '',
	caps        TEXT NOT NULL DEFAULT '',
	paired_at   INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE pair_codes (
	code_hash TEXT PRIMARY KEY,
	expires   INTEGER NOT NULL
) WITHOUT ROWID;
`}}

// Node is a paired device as the store knows it.
type Node struct {
	Name     string    `json:"name"`
	Platform string    `json:"platform"`
	Caps     []string  `json:"caps"`
	PairedAt time.Time `json:"paired_at"`
	LastSeen time.Time `json:"last_seen"`
}

// Store keeps paired nodes and pairing codes in nodes.db.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// ErrBadCode means a pairing code is unknown, used or expired.
var ErrBadCode = errors.New("the pairing code is wrong, used or expired; run `hk node pair` for a new one")

var nodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidName reports whether name can name a node: letters, digits, '.',
// '_' and '-', up to 63 characters.
func ValidName(name string) bool { return nodeName.MatchString(name) }

// OpenStore opens (creating) nodes.db at path.
func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := store.Open(ctx, path, migrations)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// codeAlphabet leaves out letters and digits that read alike (0/O, 1/I/L).
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewCode makes a pairing code that works once, until ttl passes. Codes
// are ten characters from a 31-letter alphabet (about 50 bits), shown as
// XXXXX-XXXXX; only their hash is stored.
func (s *Store) NewCode(ctx context.Context, ttl time.Duration) (string, time.Time, error) {
	var code strings.Builder
	b := make([]byte, 1)
	limit := 256 - 256%len(codeAlphabet) // reject the tail so every letter is equally likely
	for n := 0; n < 10; {
		if _, err := rand.Read(b); err != nil {
			return "", time.Time{}, err
		}
		if int(b[0]) >= limit {
			continue
		}
		if n == 5 {
			code.WriteByte('-')
		}
		code.WriteByte(codeAlphabet[int(b[0])%len(codeAlphabet)])
		n++
	}
	now := s.now()
	expires := now.Add(ttl)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pair_codes WHERE expires <= ?`, now.UnixMilli()); err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO pair_codes (code_hash, expires) VALUES (?, ?)`,
		hashCode(code.String()), expires.UnixMilli()); err != nil {
		return "", time.Time{}, err
	}
	return code.String(), expires, nil
}

// normalizeCode accepts a code typed in any case, with or without its dash
// and spaces.
func normalizeCode(code string) string {
	code = strings.ToUpper(code)
	return strings.NewReplacer("-", "", " ", "").Replace(code)
}

func hashCode(code string) string { return hashSecret("code\x00" + normalizeCode(code)) }

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Redeem spends a pairing code on a node called name and returns the
// node's new secret. Pairing a name again replaces its secret, so the old
// device can no longer connect.
func (s *Store) Redeem(ctx context.Context, code, name, platform string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("node name %q: use letters, digits, '.', '_' or '-' (up to 63)", name)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM pair_codes WHERE code_hash = ? AND expires > ?`,
		hashCode(code), s.now().UnixMilli())
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", ErrBadCode
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes (name, secret_hash, platform, paired_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET secret_hash = excluded.secret_hash, platform = excluded.platform,
			paired_at = excluded.paired_at, caps = '', last_seen = 0`,
		name, hashSecret(secret), platform, s.now().UnixMilli()); err != nil {
		return "", err
	}
	return secret, tx.Commit()
}

// Verify reports whether secret is name's current secret. Hashes compare
// in constant time.
func (s *Store) Verify(ctx context.Context, name, secret string) (bool, error) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT secret_hash FROM nodes WHERE name = ?`, name).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(hashSecret(secret))) == 1, nil
}

// Seen records what a node offered when it connected.
func (s *Store) Seen(ctx context.Context, name, platform string, caps []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET platform = CASE WHEN ? = '' THEN platform ELSE ? END, caps = ?, last_seen = ? WHERE name = ?`,
		platform, platform, strings.Join(caps, ","), s.now().UnixMilli(), name)
	return err
}

// List returns every paired node, by name.
func (s *Store) List(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, platform, caps, paired_at, last_seen FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var caps string
		var paired, seen int64
		if err := rows.Scan(&n.Name, &n.Platform, &caps, &paired, &seen); err != nil {
			return nil, err
		}
		if caps != "" {
			n.Caps = strings.Split(caps, ",")
		}
		n.PairedAt = time.UnixMilli(paired)
		if seen > 0 {
			n.LastSeen = time.UnixMilli(seen)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Revoke forgets a node, so its secret stops working. It reports whether
// there was such a node.
func (s *Store) Revoke(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// sameSecret compares two secrets in constant time.
func sameSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(a)), []byte(hashSecret(b))) == 1
}
