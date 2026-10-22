-- The admin auth instance's tables (packages/auth owns them). The admin app
-- only reads the 2FA failure counters and deletes admins it removes.

-- name: TwoFactorFailuresSince :many
SELECT tf."userId" AS admin_id, u."email", tf."failedVerificationCount" AS failures
FROM "twoFactor" tf
JOIN "user" u ON u."id" = tf."userId"
WHERE tf."updatedAt" >= sqlc.arg(since) AND tf."failedVerificationCount" > 0
ORDER BY tf."updatedAt";

-- name: DeleteAdmin :execrows
-- Sessions, accounts and 2FA rows cascade.
DELETE FROM "user" WHERE "id" = $1;
