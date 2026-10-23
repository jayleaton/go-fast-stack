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
	Dump(ctx context.Context, url string, writer io.Writer) error
	// Restore loads a custom-format dump from r; clean drops existing
	// objects first (pg_restore --clean --if-exists).
	Restore(ctx context.Context, url string, reader io.Reader, clean bool) error
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
func (service *Service) Backup(ctx context.Context, db, url string) (Result, error) {
	if len(service.Keys.Current) < MinKeyLength {
		return Result{}, errors.New("backup: BACKUP_ENCRYPTION_KEY must be at least 32 characters")
	}
	start := service.now()
	key := Key(db, start)
	tmp, err := os.CreateTemp("", "backup-*.age")
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()

	hash := sha256.New()
	counter := &countWriter{}
	sink := io.MultiWriter(tmp, hash, counter)
	rcp, err := age.NewScryptRecipient(service.Keys.Current)
	if err != nil {
		return Result{}, fmt.Errorf("backup: encryption key: %w", err)
	}
	if service.WorkFactor > 0 {
		rcp.SetWorkFactor(service.WorkFactor)
	}
	enc, err := age.Encrypt(sink, rcp)
	if err != nil {
		return Result{}, fmt.Errorf("backup: encrypt: %w", err)
	}
	if err := service.Postgres.Dump(ctx, url, enc); err != nil {
		return Result{}, fmt.Errorf("backup: dump %s: %w", db, err)
	}
	if err := enc.Close(); err != nil {
		return Result{}, fmt.Errorf("backup: encrypt: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if err := service.Store.Put(ctx, key, tmp, counter.count, sum); err != nil {
		return Result{}, err
	}
	res := Result{Key: key, Size: counter.count, Fingerprint: sum[:16]}
	service.log().Info("backup: stored", "id", res.Key, "bytes", res.Size, "sha256", res.Fingerprint, "took", service.now().Sub(start).Round(time.Millisecond))
	return res, nil
}

// List returns db's backups, newest first ("" lists every database).
func (service *Service) List(ctx context.Context, db string) ([]Object, error) {
	prefix := ""
	if db != "" {
		prefix = db + "/"
	}
	objs, err := service.Store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	objs = slices.DeleteFunc(objs, func(object Object) bool { _, _, err := ParseKey(object.Key); return err != nil })
	slices.SortFunc(objs, func(left, right Object) int {
		_, ta, _ := ParseKey(left.Key)
		_, tb, _ := ParseKey(right.Key)
		return tb.Compare(ta)
	})
	return objs, nil
}

// Prune deletes db's backups outside Retention and returns them.
func (service *Service) Prune(ctx context.Context, db string) ([]Object, error) {
	objs, err := service.Store.List(ctx, db+"/")
	if err != nil {
		return nil, err
	}
	expired := service.retention().Expired(objs, service.now())
	for _, object := range expired {
		if err := service.Store.Delete(ctx, object.Key); err != nil {
			return nil, err
		}
		service.log().Info("backup: pruned", "id", object.Key, "bytes", object.Size)
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
func (service *Service) Restore(ctx context.Context, id, url string, options RestoreOptions) error {
	if !options.Yes {
		return ErrNotConfirmed
	}
	if _, _, err := ParseKey(id); err != nil {
		return err
	}
	tables, err := service.Postgres.Tables(ctx, url)
	if err != nil {
		return fmt.Errorf("backup: inspect target: %w", err)
	}
	if tables > 0 && !options.Force {
		return fmt.Errorf("%w (%d objects)", ErrNotEmpty, tables)
	}
	body, err := service.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	var ids []age.Identity
	for _, key := range append([]string{service.Keys.Current}, service.Keys.Previous...) {
		if key == "" {
			continue
		}
		ident, err := age.NewScryptIdentity(key)
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
	start := service.now()
	if err := service.Postgres.Restore(ctx, url, plain, tables > 0); err != nil {
		return fmt.Errorf("backup: restore %s: %w", id, err)
	}
	service.log().Info("backup: restored", "id", id, "replaced_objects", tables, "took", service.now().Sub(start).Round(time.Millisecond))
	return nil
}

func (service *Service) now() time.Time {
	if service.Now != nil {
		return service.Now()
	}
	return time.Now()
}

func (service *Service) retention() Retention {
	if service.Retention.Daily <= 0 && service.Retention.Weekly <= 0 {
		return DefaultRetention
	}
	return service.Retention
}

func (service *Service) log() *slog.Logger {
	if service.Logger != nil {
		return service.Logger
	}
	return slog.Default()
}

type countWriter struct{ count int64 }

func (writer *countWriter) Write(data []byte) (int, error) {
	writer.count += int64(len(data))
	return len(data), nil
}
