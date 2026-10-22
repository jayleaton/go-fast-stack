package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/go-fast-stack/packages/database"
	"github.com/jayleaton/go-fast-stack/packages/database/dbtest"
	"github.com/jayleaton/go-fast-stack/packages/database/model"
)

func seedUser(t *testing.T, pool *pgxpool.Pool, id, email string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO "user" (id, name, email) VALUES ($1, 'Ada Lovelace', $2)`, id, email)
	if err != nil {
		t.Fatal(err)
	}
}

func TestUserRepositoryGet(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	repo := database.NewUserRepository(pool)
	ctx := context.Background()
	seedUser(t, pool, "u1", "ada@example.com")

	u, err := repo.GetByID(ctx, "u1")
	if err != nil || u.Email != "ada@example.com" || u.Name != "Ada Lovelace" {
		t.Fatalf("GetByID = %+v, %v", u, err)
	}
	if _, err := repo.GetByID(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}
}

func TestUserRepositoryDataSummary(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	repo := database.NewUserRepository(pool)
	ctx := context.Background()
	seedUser(t, pool, "u1", "ada@example.com")
	seedUser(t, pool, "u2", "eve@example.com")
	// Two live sessions and one expired for u1, one for u2.
	_, err := pool.Exec(ctx, `INSERT INTO "session" (id, token, "userId", "expiresAt") VALUES
		('s1', 't1', 'u1', now() + interval '1 day'),
		('s2', 't2', 'u1', now() + interval '1 day'),
		('s3', 't3', 'u1', now() - interval '1 day'),
		('s4', 't4', 'u2', now() + interval '1 day')`)
	if err != nil {
		t.Fatal(err)
	}

	sum, err := repo.DataSummary(ctx, "u1")
	if err != nil || sum.User.ID != "u1" || sum.Sessions != 2 {
		t.Fatalf("DataSummary = %+v, %v; want u1 with 2 live sessions", sum, err)
	}
	if _, err := repo.DataSummary(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}
}

func TestUserRepositoryUpdateProfile(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	repo := database.NewUserRepository(pool)
	ctx := context.Background()
	seedUser(t, pool, "u1", "ada@example.com")
	name, image := "Ada", "https://example.com/ada.png"

	u, err := repo.UpdateProfile(ctx, "u1", model.UserProfileUpdate{Name: &name, SetImage: true, Image: &image})
	if err != nil || u.Name != "Ada" || u.Image == nil || *u.Image != image {
		t.Fatalf("set name+image = %+v, %v", u, err)
	}
	if !u.UpdatedAt.After(u.CreatedAt) && !u.UpdatedAt.Equal(u.CreatedAt) {
		t.Fatalf("updatedAt %v before createdAt %v", u.UpdatedAt, u.CreatedAt)
	}

	u, err = repo.UpdateProfile(ctx, "u1", model.UserProfileUpdate{})
	if err != nil || u.Name != "Ada" || u.Image == nil {
		t.Fatalf("empty update changed fields: %+v, %v", u, err)
	}

	u, err = repo.UpdateProfile(ctx, "u1", model.UserProfileUpdate{SetImage: true})
	if err != nil || u.Image != nil {
		t.Fatalf("clear image = %+v, %v", u, err)
	}

	if _, err := repo.UpdateProfile(ctx, "missing", model.UserProfileUpdate{Name: &name}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}
}

func TestUserRepositoryDelete(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	repo := database.NewUserRepository(pool)
	ctx := context.Background()
	seedUser(t, pool, "u1", "ada@example.com")

	if err := repo.Delete(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetByID(ctx, "u1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, "u1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
}
