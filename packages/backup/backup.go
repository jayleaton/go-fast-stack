// Package backup takes, keeps and restores encrypted Postgres backups.
//
// A backup is `pg_dump -Fc` (compressed), encrypted with age under a
// passphrase (BACKUP_ENCRYPTION_KEY), stored as <db>/<time>.dump.age in a
// Store: an S3-compatible bucket (Railway storage buckets by default) or a
// local directory. Prune keeps Retention. Restore refuses without an explicit
// yes, and refuses a database that already has tables unless forced.
//
// Logs carry the backup id, size and a fingerprint (SHA-256 of the
// encrypted object, as stored), never database contents, URLs or keys.
// Restoring needs only the stored object and the passphrase: in an emergency,
// `age -d backup.dump.age | pg_restore -d ...` works without this package.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"filippo.io/age"
)

// MinKeyLength is the shortest BACKUP_ENCRYPTION_KEY accepted, in characters.
const MinKeyLength = 32

// ErrNotFound: no backup with that id.
var ErrNotFound = errors.New("backup: not found")

// Postgres runs the database side: pg_dump / pg_restore and a table count.
// PgTools is the real one.
type Postgres interface {
	Dump(ctx context.Context, url string, w io.Writer) error
	// Restore loads a custom-format dump from r; clean drops existing
	// objects first (pg_restore --clean --if-exists).
	Restore(ctx context.Context, url string, r io.Reader, clean bool) error
	// Tables counts user tables (outside pg_catalog/information_schema).
	Tables(ctx context.Context, url string) (int, error)
}

// Keys are the encryption passphrases: Current encrypts, Previous still
// decrypt (backups taken before a rotation of BACKUP_ENCRYPTION_KEY).
type Keys struct {
	Current  string
	Previous []string
}

// Service takes and restores backups. Its zero value is not usable.
type Service struct {
	Store     Store
	Postgres  Postgres
	Keys      Keys
	Retention Retention
	Logger    *slog.Logger
	Now       func() time.Time // default time.Now
	// WorkFactor is age's scrypt cost (log2 N); 0 uses age's default (18,
	// about a second). Tests lower it.
	WorkFactor int
}

// Result describes one stored backup.
type Result struct {
	Key         string
	Size        int64
	Fingerprint string // first 16 hex digits of the stored object's SHA-256
}

// Backup dumps db (a name: "app", "admin") from url, encrypts it, stores it
// and logs its id, size and fingerprint.
func (s *Service) Backup(ctx context.Context, db, url string) (Result, error) {
	if len(s.Keys.Current) < MinKeyLength {
		return Result{}, errors.New("backup: BACKUP_ENCRYPTION_KEY must be at least 32 characters")
	}
	start := s.now()
	key := Key(db, start)
	tmp, err := os.CreateTemp("", "backup-*.age")
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()

	hash := sha256.New()
	counter := &countWriter{}
	sink := io.MultiWriter(tmp, hash, counter)
	rcp, err := age.NewScryptRecipient(s.Keys.Current)
	if err != nil {
		return Result{}, fmt.Errorf("backup: encryption key: %w", err)
	}
	if s.WorkFactor > 0 {
		rcp.SetWorkFactor(s.WorkFactor)
	}
	enc, err := age.Encrypt(sink, rcp)
	if err != nil {
		return Result{}, fmt.Errorf("backup: encrypt: %w", err)
	}
	if err := s.Postgres.Dump(ctx, url, enc); err != nil {
		return Result{}, fmt.Errorf("backup: dump %s: %w", db, err)
	}
	if err := enc.Close(); err != nil {
		return Result{}, fmt.Errorf("backup: encrypt: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if err := s.Store.Put(ctx, key, tmp, counter.n, sum); err != nil {
		return Result{}, err
	}
	res := Result{Key: key, Size: counter.n, Fingerprint: sum[:16]}
	s.log().Info("backup: stored", "id", res.Key, "bytes", res.Size, "sha256", res.Fingerprint, "took", s.now().Sub(start).Round(time.Millisecond))
	return res, nil
}

// List returns db's backups, newest first ("" lists every database).
func (s *Service) List(ctx context.Context, db string) ([]Object, error) {
	prefix := ""
	if db != "" {
		prefix = db + "/"
	}
	objs, err := s.Store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	objs = slices.DeleteFunc(objs, func(o Object) bool { _, _, err := ParseKey(o.Key); return err != nil })
	slices.SortFunc(objs, func(a, b Object) int {
		_, ta, _ := ParseKey(a.Key)
		_, tb, _ := ParseKey(b.Key)
		return tb.Compare(ta)
	})
	return objs, nil
}

// Prune deletes db's backups outside Retention and returns them.
func (s *Service) Prune(ctx context.Context, db string) ([]Object, error) {
	objs, err := s.Store.List(ctx, db+"/")
	if err != nil {
		return nil, err
	}
	expired := s.retention().Expired(objs, s.now())
	for _, o := range expired {
		if err := s.Store.Delete(ctx, o.Key); err != nil {
			return nil, err
		}
		s.log().Info("backup: pruned", "id", o.Key, "bytes", o.Size)
	}
	return expired, nil
}

// RestoreOptions guard a restore.
type RestoreOptions struct {
	Yes   bool // required: a restore replaces data
	Force bool // restore over a database that already has tables (drops what the backup has)
}

// ErrNotConfirmed: Restore was called without Yes.
var ErrNotConfirmed = errors.New("backup: restore replaces data: pass --yes to confirm")

// ErrNotEmpty: the target has tables and Force was not set.
var ErrNotEmpty = errors.New("backup: the target database is not empty: pass --force to replace everything in it with the backup")

// Restore loads backup id into the database at url. With Force, a target
// that has objects is replaced entirely by the backup.
func (s *Service) Restore(ctx context.Context, id, url string, o RestoreOptions) error {
	if !o.Yes {
		return ErrNotConfirmed
	}
	if _, _, err := ParseKey(id); err != nil {
		return err
	}
	n, err := s.Postgres.Tables(ctx, url)
	if err != nil {
		return fmt.Errorf("backup: inspect target: %w", err)
	}
	if n > 0 && !o.Force {
		return fmt.Errorf("%w (%d objects)", ErrNotEmpty, n)
	}
	body, err := s.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	var ids []age.Identity
	for _, k := range append([]string{s.Keys.Current}, s.Keys.Previous...) {
		if k == "" {
			continue
		}
		ident, err := age.NewScryptIdentity(k)
		if err != nil {
			return fmt.Errorf("backup: encryption key: %w", err)
		}
		ids = append(ids, ident)
	}
	if len(ids) == 0 {
		return errors.New("backup: BACKUP_ENCRYPTION_KEY is required to restore")
	}
	plain, err := age.Decrypt(body, ids...)
	if err != nil {
		return fmt.Errorf("backup: decrypt %s (wrong BACKUP_ENCRYPTION_KEY? list the old one in BACKUP_ENCRYPTION_KEY_PREVIOUS): %w", id, err)
	}
	start := s.now()
	if err := s.Postgres.Restore(ctx, url, plain, n > 0); err != nil {
		return fmt.Errorf("backup: restore %s: %w", id, err)
	}
	s.log().Info("backup: restored", "id", id, "replaced_objects", n, "took", s.now().Sub(start).Round(time.Millisecond))
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) retention() Retention {
	if s.Retention.Daily <= 0 && s.Retention.Weekly <= 0 {
		return DefaultRetention
	}
	return s.Retention
}

func (s *Service) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }
