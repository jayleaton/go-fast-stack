# Object storage and image ingestion (optional)

The web app can keep files in any S3-compatible bucket: a Railway storage bucket, Cloudflare R2, AWS S3, Backblaze B2 or MinIO. It is **off unless configured**. With no `STORAGE_S3_BUCKET`, the server boots as usual, logs `storage: off`, and the image routes are not mounted (they answer 404 like any unknown path).

One example uses it: a signed-in user uploads an image, or names a URL for the server to fetch one from. The server checks it, turns it upright, shrinks it to fit 2048 × 2048, strips its metadata (EXIF, GPS), re-encodes it as WebP and stores it under that user's own prefix (`users/<user id>/images/<id>.webp`). Copy the pattern for your own files, or delete it (see the end of this page).

## Turn it on

Set the four storage secrets together. All four or none: a partial set fails boot with the missing names.

| Name | Live secret | Example |
|---|---|---|
| `STORAGE_S3_ENDPOINT` | yes | `https://t3.storageapi.dev` (Railway), `https://<account>.r2.cloudflarestorage.com` (R2), `https://s3.us-east-1.amazonaws.com`, `http://127.0.0.1:9000` (MinIO) |
| `STORAGE_S3_BUCKET` | yes | the bucket name |
| `STORAGE_S3_ACCESS_KEY_ID` | yes | |
| `STORAGE_S3_SECRET_ACCESS_KEY` | yes | |
| `STORAGE_S3_REGION` | no (env) | empty is `auto` (Railway, R2); AWS needs the bucket's region; MinIO uses `us-east-1` |
| `STORAGE_S3_PATH_STYLE` | no (env) | `true` for MinIO (`endpoint/bucket/key`); default `false` (`bucket.endpoint/key`) |

The bucket and keys are live secrets (see [docs/secrets.md](secrets.md)): with `SECRETS_PROVIDER=doppler` they come from Doppler, so a bucket move or a key rotation needs no redeploy (see "Rotate keys" below). The region and addressing style are plain env.

At boot the server makes one signed `HEAD` on the bucket. Wrong keys or a missing bucket fail the boot with the fingerprints of the four values (never the values), the way an unreachable database does, so a bad deploy never goes live.

**Railway:** add a storage bucket to the project and copy its credentials into those four names (or into Doppler). **Locally**, any S3 server works, for example MinIO:

```sh
docker run -d --name minio -p 127.0.0.1:9000:9000 -e MINIO_ROOT_USER=dev -e MINIO_ROOT_PASSWORD=dev-secret-123 minio/minio server /data
docker run --rm --network host --entrypoint sh minio/mc -c 'mc alias set l http://127.0.0.1:9000 dev dev-secret-123 && mc mb l/app'
# .env
STORAGE_S3_ENDPOINT=http://127.0.0.1:9000
STORAGE_S3_BUCKET=app
STORAGE_S3_ACCESS_KEY_ID=dev
STORAGE_S3_SECRET_ACCESS_KEY=dev-secret-123
STORAGE_S3_REGION=us-east-1
STORAGE_S3_PATH_STYLE=true
```

## Use it

JSON API (mobile app and servers; signed in, bearer token):

| Route | Does |
|---|---|
| `POST /api/core/v1/images` | the raw image is the body (any `Content-Type`: the bytes decide); returns `{id, contentType, width, height, size}` |
| `POST /api/core/v1/images/import` | `{"url": "https://..."}`: the server fetches it, then as above |
| `GET /api/core/v1/images/{id}` | the stored WebP |
| `DELETE /api/core/v1/images/{id}` | removes it (also when already gone) |

```sh
curl -sS -H "Authorization: Bearer $TOKEN" --data-binary @photo.jpg http://<host>:3000/api/core/v1/images
curl -sS -H "Authorization: Bearer $TOKEN" -d '{"url":"https://example.com/cat.avif"}' http://<host>:3000/api/core/v1/images/import
```

Accepted: JPEG, PNG, GIF (first frame), WebP (first frame) and AVIF, up to 10 MB and 50 megapixels. Limits live in `apps/web/internal/services/images.go`. Each user only reaches their own prefix: another user's id is not found.

## What keeps it safe

- **Fetching a URL cannot reach your network.** `packages/storage/safefetch` checks every IP it is about to dial, after DNS and on every redirect hop, against `services.PublicAddr`: loopback, private, carrier-grade NAT (Tailscale), link-local and cloud metadata (`169.254.169.254`, `fd00:ec2::254`), multicast, reserved and documentation ranges are refused, and so are IPv6 forms that embed them (`::ffff:`, NAT64, 6to4). Because the check runs at dial time, a name that resolves to a public IP when checked and to `127.0.0.1` when dialled (DNS rebinding) is refused too. URLs must be http(s) on port 80/443 without credentials or numeric-host tricks (`2130706433`, `0x7f.1`); at most 3 redirects, never https to http; 10 MB and 20 s for the whole fetch; no proxy, ever.
- **The bytes decide the type.** The format is sniffed from magic bytes; the declared `Content-Type` is ignored. SVG, HTML and HEIC are refused.
- **Bombs are refused before decoding.** The pixel count is read from the header, and at most two images decode at once (`imageConcurrency` in `cmd/server/storage.go`).
- **Memory per upload** (peak RSS above an idle server, measured): about 30 MB for a 12 MP photo, 70 MB for a 48 MP photo, 120 MB for two 12 MP photos at once. The resize works in bands of rows with at most 8 MiB of scratch, and gives the same pixels as `golang.org/x/image/draw`'s Catmull-Rom, which needs 198 MB of scratch for a 12 MP photo; so a 512 MB service has room for uploads. Set `GOMEMLIMIT` (for example to 80% of the service's memory) so Go hands freed memory back to the OS sooner.
- **Output is re-encoded.** What lands in the bucket is a fresh WebP, never the upload, and it is served with `Content-Security-Policy: sandbox`.

## Pure Go, static binary

The codecs are pure Go: `github.com/gen2brain/webp` (libwebp transpiled to Go) and `github.com/gen2brain/avif` (libavif and dav1d in WebAssembly under wazero). They add 7.6 MB to the binary (24.5 MB vs 16.9 MB, linux/amd64): AVIF (with the wazero runtime) 4.1 MB, WebP 3.1 MB, the rest of `imaging` 0.4 MB; the S3 client and safe fetch are under 0.1 MB. Nothing runs at boot: AVIF compiles its WebAssembly module on the first AVIF image (about 1.4 s and 6.5 MB, once). Without `-tags nodynamic` they link purego to try a system libwebp/libavif, and a `CGO_ENABLED=0` binary comes out **dynamically linked**, which a distroless/static image cannot run. `make build` and the `Dockerfile` pass the tag, and the Docker build fails if it is missing. Plain `go run`/`go test` still work without it.

## Rotate keys

With `SECRETS_PROVIDER=doppler`, change the values in Doppler; no restart, no redeploy. On its next poll (`SECRETS_REFRESH_INTERVAL`) the server builds a client for the new set and makes one real signed call with it (`HEAD` on the bucket). Only if that works does it switch, atomically: uploads already running finish on the old client, new ones use the new bucket and keys. The log shows `storage: bucket/keys rotated from=… to=…` with fingerprints, never values.

If the probe fails (a typo, a key without access, a bucket that is not there yet), the server keeps the old client, logs `consumer rejected change, keeping its previous client` with the fingerprints and the reason (`access denied: head 403 Forbidden`), and tries again after every later fetch, so fixing the value in Doppler is enough. A partial set (some of the four names missing) is refused before it is published. Removing all four upstream keeps the current client until a restart; turning storage on upstream while it was off at boot needs a restart to mount the routes (it is logged).

Rotate a key without downtime:

1. Create a second key for the bucket (Railway, R2 and S3 allow two at once).
2. Put it in `STORAGE_S3_ACCESS_KEY_ID` and `STORAGE_S3_SECRET_ACCESS_KEY` in Doppler, and wait for the `rotated` log line on every instance.
3. Revoke the old key.

Moving to another bucket works the same way (copy the objects first: the server does not migrate them). With `SECRETS_PROVIDER=env`, change the env and restart.

## Delete it

Delete `packages/storage`, `docs/storage.md`, and in `apps/web`: `cmd/server/storage.go`, `internal/services/{images,outbound}*.go`, `internal/services/mocks/images.go`, `internal/api/images*.go`, `internal/api/mocks/images.go`, `internal/validate/images.go`, `internal/config/storage.go` and `internal/server/images.go`. Then remove the `newImages` call in `cmd/server/main.go`, `Images` from `server.Deps`, the `Images*` routes in `config/routes.go`, `storageSecrets` and `validateStorage` in `config/secrets.go`, `Storage` in `config.Config`, `AuthenticatedLimit`/`AuthenticatedFile` in `api/route.go` if nothing else uses them, the `storage` depguard rule and its probes, the `STORAGE_S3_*` lines in `.env.example`, and `-tags nodynamic` (the tag is harmless if you keep it). `go mod tidy` drops the codecs.

Deleting a user's account does not remove their images; if you keep the example, delete the `users/<id>/` prefix in `UserService.DeleteAccount`.
