-- Read-only over the main app's database: packages/auth owns these tables.

-- name: UserOverview :one
SELECT
    count(*)                                                     AS total_users,
    count(*) FILTER (WHERE "emailVerified")                      AS verified_users,
    count(*) FILTER (WHERE "createdAt" > now() - interval '7 days') AS new_users_7d,
    (SELECT count(*) FROM "session" WHERE "expiresAt" > now())   AS active_sessions
FROM "user";
