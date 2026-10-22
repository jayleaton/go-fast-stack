package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/database/model"
	"github.com/jayleaton/ai-patterns-go/packages/database/sqlc"
)

// UserRepository reads and updates the non-credential columns of the Better
// Auth "user" table. packages/auth owns the table and its credentials.
type UserRepository struct {
	q *sqlc.Queries
}

// NewUserRepository returns a repository backed by pool.
func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{q: sqlc.New(pool)}
}

// GetUserByID returns model.ErrNotFound when no user has id.
func (r *UserRepository) GetUserByID(ctx context.Context, id string) (model.User, error) {
	return one(r.q.GetUserByID(ctx, id))
}

// GetUserByEmail returns model.ErrNotFound when no user has email.
func (r *UserRepository) GetUserByEmail(ctx context.Context, email string) (model.User, error) {
	return one(r.q.GetUserByEmail(ctx, email))
}

// UpdateUserProfile applies u and returns the updated user.
func (r *UserRepository) UpdateUserProfile(ctx context.Context, id string, u model.UserProfileUpdate) (model.User, error) {
	return one(r.q.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
		ID: id, Name: u.Name, SetImage: u.SetImage, Image: u.Image,
	}))
}

// DeleteUser deletes the user; sessions, accounts and app rows cascade.
func (r *UserRepository) DeleteUser(ctx context.Context, id string) error {
	n, err := r.q.DeleteUser(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return model.ErrNotFound
	}
	return nil
}

// GetUserDataSummary lists the data stored for a user.
func (r *UserRepository) GetUserDataSummary(ctx context.Context, id string) (model.UserDataSummary, error) {
	u, err := r.GetUserByID(ctx, id)
	if err != nil {
		return model.UserDataSummary{}, err
	}
	return model.UserDataSummary{User: u}, nil
}

func one(u sqlc.User, err error) (model.User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, model.ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	return model.User(u), nil
}
