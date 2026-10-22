-- name: GetUserByID :one
SELECT * FROM "user" WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM "user" WHERE email = $1;

-- name: UpdateUserProfile :one
UPDATE "user"
SET name        = coalesce(sqlc.narg(name), name),
    image       = CASE WHEN sqlc.arg(set_image)::boolean THEN sqlc.narg(image) ELSE image END,
    "updatedAt" = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteUser :execrows
DELETE FROM "user" WHERE id = $1;
