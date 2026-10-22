-- Better Auth admin plugin fields and twoFactor plugin table.

-- +goose Up
ALTER TABLE "user"
    ADD COLUMN "role"             text NOT NULL DEFAULT 'user',
    ADD COLUMN "banned"           boolean NOT NULL DEFAULT false,
    ADD COLUMN "banReason"        text,
    ADD COLUMN "banExpires"       timestamptz,
    ADD COLUMN "twoFactorEnabled" boolean NOT NULL DEFAULT false;

-- One row per user. "secret" is AES-GCM encrypted with a key derived from the
-- auth secret; "backupCodes" is a JSON array of keyed hashes, each removed when
-- used. "lastUsedStep" is the last accepted TOTP time step, so a code cannot
-- be replayed within its validity window.
CREATE TABLE "twoFactor" (
    "id"                      text PRIMARY KEY,
    "secret"                  text NOT NULL,
    "backupCodes"             text NOT NULL,
    "userId"                  text NOT NULL UNIQUE REFERENCES "user" ("id") ON DELETE CASCADE,
    "verified"                boolean NOT NULL DEFAULT false,
    "failedVerificationCount" integer NOT NULL DEFAULT 0,
    "lockedUntil"             timestamptz,
    "lastUsedStep"            bigint NOT NULL DEFAULT 0,
    "createdAt"               timestamptz NOT NULL DEFAULT now(),
    "updatedAt"               timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE "twoFactor";
ALTER TABLE "user"
    DROP COLUMN "twoFactorEnabled",
    DROP COLUMN "banExpires",
    DROP COLUMN "banReason",
    DROP COLUMN "banned",
    DROP COLUMN "role";
