-- The admin auth instance's tables (packages/auth owns them). The admin app
-- only deletes admins it removes.

-- name: DeleteAdmin :execrows
-- Sessions, accounts and 2FA rows cascade.
DELETE FROM "user" WHERE "id" = $1;
