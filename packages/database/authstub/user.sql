-- Stand-in for the Better Auth "user" table until packages/auth lands.
-- packages/auth owns the real table; this file only feeds sqlc, tests and the dev auth fake.
CREATE TABLE IF NOT EXISTS "user" (
    "id"            text PRIMARY KEY,
    "name"          text NOT NULL,
    "email"         text NOT NULL UNIQUE,
    "emailVerified" boolean NOT NULL DEFAULT false,
    "image"         text,
    "createdAt"     timestamptz NOT NULL DEFAULT now(),
    "updatedAt"     timestamptz NOT NULL DEFAULT now()
);
