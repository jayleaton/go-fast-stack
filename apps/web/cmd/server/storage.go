package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/server"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/secrets/reveal"
	"github.com/jayleaton/go-fast-stack/packages/storage"
	"github.com/jayleaton/go-fast-stack/packages/storage/imaging"
)

// imageConcurrency bounds how many images are decoded at once. Measured peak
// RSS above an idle server: about 30 MB for one 12 MP photo, 70 MB for one
// 48 MP photo (the decoded pixels and the WebP encoder; resizing uses at most
// 8 MiB), 120 MB for two 12 MP photos at once (docs/storage.md).
const imageConcurrency = 2

// newImages builds the image endpoints over object storage, or returns nil
// (routes not mounted) when STORAGE_S3_BUCKET is unset. A bucket that the
// keys cannot reach fails boot, like a database that cannot be reached.
// The bucket and keys are live secrets: a new set is probed with one real
// call and swapped in only if it works (docs/storage.md).
func newImages(ctx context.Context, cfg config.Config, gate *access.Gate, log *slog.Logger) (*api.Images, error) {
	creds, err := cfg.Secrets.Storage()
	if err != nil {
		return nil, err
	}
	if !creds.Configured() {
		log.Info("storage: off (STORAGE_S3_BUCKET not set); image routes not mounted")
		cfg.Secrets.OnStorage(func(credentials config.StorageCredentials) error {
			if credentials.Configured() {
				log.Warn("storage: STORAGE_S3_* set upstream but storage was off at boot; restart to mount the image routes", "storage", credentials.Fingerprint())
			}
			return nil
		})
		return nil, nil
	}
	client, err := storage.New(storageConfig(cfg, creds))
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, rotationTimeout)
	defer cancel()
	if err := client.Probe(probeCtx); err != nil {
		return nil, fmt.Errorf("storage: bucket probe failed (%s): %w", creds.Fingerprint(), err)
	}
	log.Info("storage: bucket reachable", "storage", creds.Fingerprint())
	live := storage.NewLive(client)
	applied := creds.Fingerprint()
	cfg.Secrets.OnStorage(func(credentials config.StorageCredentials) error {
		next := credentials.Fingerprint()
		if next == applied {
			return nil
		}
		if !credentials.Configured() { // removed upstream: keep serving from the bucket we have
			log.Warn("storage: STORAGE_S3_* removed upstream; keeping the current bucket client until restart", "storage", applied)
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, rotationTimeout) // shutdown aborts a rotation in progress
		defer cancel()
		if err := live.Swap(ctx, storageConfig(cfg, credentials)); err != nil {
			return fmt.Errorf("storage: new bucket/keys %s failed their probe, keeping %s: %w", next, applied, err) // retried after the next fetch
		}
		log.Info("storage: bucket/keys rotated", "from", applied, "to", next)
		applied = next
		return nil
	})
	svc := services.NewImageService(
		server.NewObjectStore(live),
		server.NewImageFetcher(server.NewSafeFetch(cfg.AppName)),
		server.NewImageCodec(imaging.New(imageConcurrency)),
	)
	return api.NewImages(svc, gate), nil
}

func storageConfig(cfg config.Config, credentials config.StorageCredentials) storage.Config {
	return storage.Config{
		Endpoint:        reveal.Value(credentials.Endpoint),
		Bucket:          reveal.Value(credentials.Bucket),
		AccessKeyID:     reveal.Value(credentials.AccessKeyID),
		SecretAccessKey: reveal.Value(credentials.SecretAccessKey),
		Region:          cfg.Storage.Region,
		PathStyle:       cfg.Storage.PathStyle,
	}
}
