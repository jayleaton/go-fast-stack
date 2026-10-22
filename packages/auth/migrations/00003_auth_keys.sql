-- Key state for live AUTH_SECRET rotation (keyring.go). No secret is stored.
--
-- "secret:<kid>" rows: one per auth secret this database has seen. "retiredAt"
-- is when it stopped being the current secret (NULL while current); every
-- replica accepts a retired secret until retiredAt + the grace period.
--
-- "data:<cookie prefix>:<purpose>" rows: the keys protecting data at rest
-- (TOTP secrets, backup-code hashes), AES-GCM wrapped by the auth secret named
-- by "kid". Rotating the auth secret rewraps these rows; the data itself is
-- never re-encrypted.

-- +goose Up
CREATE TABLE "authKey" (
    "id"        text PRIMARY KEY,
    "kid"       text,
    "wrapped"   text,
    "retiredAt" timestamptz,
    "createdAt" timestamptz NOT NULL DEFAULT now(),
    "updatedAt" timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE "authKey";
