package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/database"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
	"github.com/jayleaton/ai-patterns-go/packages/database/model"
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

	byID, err := repo.GetUserByID(ctx, "u1")
	if err != nil || byID.Email != "ada@example.com" || byID.Name != "Ada Lovelace" {
		t.Fatalf("GetUserByID = %+v, %v", byID, err)
	}
	byEmail, err := repo.GetUserByEmail(ctx, "ada@example.com")
	if err != nil || byEmail.ID != "u1" {
		t.Fatalf("GetUserByEmail = %+v, %v", byEmail, err)
	}
	if _, err := repo.GetUserByID(ctx, "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}
	summary, err := repo.GetUserDataSummary(ctx, "u1")
	if err != nil || summary.User.ID != "u1" {
		t.Fatalf("GetUserDataSummary = %+v, %v", summary, err)
	}
}

func TestUserRepositoryUpdateProfile(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	repo := database.NewUserRepository(pool)
	ctx := context.Background()
	seedUser(t, pool, "u1", "ada@example.com")
	name, image := "Ada", "https://example.com/ada.png"

	u, err := repo.UpdateUserProfile(ctx, "u1", model.UserProfileUpdate{Name: &name, SetImage: true, Image: &image})
	if err != nil || u.Name != "Ada" || u.Image == nil || *u.Image != image {
		t.Fatalf("set name+image = %+v, %v", u, err)
	}
	if !u.UpdatedAt.After(u.CreatedAt) && !u.UpdatedAt.Equal(u.CreatedAt) {
		t.Fatalf("updatedAt %v before createdAt %v", u.UpdatedAt, u.CreatedAt)
	}

	u, err = repo.UpdateUserProfile(ctx, "u1", model.UserProfileUpdate{})
	if err != nil || u.Name != "Ada" || u.Image == nil {
		t.Fatalf("empty update changed fields: %+v, %v", u, err)
	}

	u, err = repo.UpdateUserProfile(ctx, "u1", model.UserProfileUpdate{SetImage: true})
	if err != nil || u.Image != nil {
		t.Fatalf("clear image = %+v, %v", u, err)
	}

	if _, err := repo.UpdateUserProfile(ctx, "missing", model.UserProfileUpdate{Name: &name}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
	}
}

func TestUserRepositoryDelete(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	repo := database.NewUserRepository(pool)
	ctx := context.Background()
	seedUser(t, pool, "u1", "ada@example.com")

	if err := repo.DeleteUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetUserByID(ctx, "u1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
	if err := repo.DeleteUser(ctx, "u1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	if err := database.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}
