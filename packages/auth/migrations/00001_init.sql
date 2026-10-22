-- Better Auth core schema (user, session, account, verification).
-- Table and column names match Better Auth 1.7 so data and feature ports carry over.

-- +goose Up
CREATE TABLE "user" (
    "id"            text PRIMARY KEY,
    "name"          text NOT NULL,
    "email"         text NOT NULL UNIQUE,
    "emailVerified" boolean NOT NULL DEFAULT false,
    "image"         text,
    "createdAt"     timestamptz NOT NULL DEFAULT now(),
    "updatedAt"     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE "session" (
    "id"        text PRIMARY KEY,
    "expiresAt" timestamptz NOT NULL,
    "token"     text NOT NULL UNIQUE,
    "createdAt" timestamptz NOT NULL DEFAULT now(),
    "updatedAt" timestamptz NOT NULL DEFAULT now(),
    "ipAddress" text,
    "userAgent" text,
    "userId"    text NOT NULL REFERENCES "user" ("id") ON DELETE CASCADE
);
CREATE INDEX "session_userId_idx" ON "session" ("userId");

CREATE TABLE "account" (
    "id"                    text PRIMARY KEY,
    "accountId"             text NOT NULL,
    "providerId"            text NOT NULL,
    "userId"                text NOT NULL REFERENCES "user" ("id") ON DELETE CASCADE,
    "accessToken"           text,
    "refreshToken"          text,
    "idToken"               text,
    "accessTokenExpiresAt"  timestamptz,
    "refreshTokenExpiresAt" timestamptz,
    "scope"                 text,
    "password"              text,
    "createdAt"             timestamptz NOT NULL DEFAULT now(),
    "updatedAt"             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX "account_userId_idx" ON "account" ("userId");
CREATE UNIQUE INDEX "account_providerId_accountId_idx" ON "account" ("providerId", "accountId");

CREATE TABLE "verification" (
    "id"         text PRIMARY KEY,
    "identifier" text NOT NULL,
    "value"      text NOT NULL,
    "expiresAt"  timestamptz NOT NULL,
    "createdAt"  timestamptz NOT NULL DEFAULT now(),
    "updatedAt"  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX "verification_identifier_idx" ON "verification" ("identifier");
CREATE INDEX "verification_expiresAt_idx" ON "verification" ("expiresAt");

-- +goose Down
DROP TABLE "verification";
DROP TABLE "account";
DROP TABLE "session";
DROP TABLE "user";
