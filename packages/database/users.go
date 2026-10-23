package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jayleaton/go-fast-stack/packages/database/model"
	"github.com/jayleaton/go-fast-stack/packages/database/sqlc"
)

// UserRepository reads and updates the non-credential columns of the Better
// Auth "user" table. packages/auth owns the table and its credentials.
//
// It implements services.UserRepository: the method set is the port the
// service declares, nothing more. Copy this file for a new repository.
type UserRepository struct {
	queries *sqlc.Queries
}

// NewUserRepository returns a repository backed by db: a *Pool (live
// DATABASE_URL rotation) or a *pgxpool.Pool.
func NewUserRepository(db sqlc.DBTX) *UserRepository {
	return &UserRepository{queries: sqlc.New(db)}
}

// GetByID returns model.ErrNotFound when no user has id.
func (repository *UserRepository) GetByID(ctx context.Context, id string) (model.User, error) {
	return one(repository.queries.GetUserByID(ctx, id))
}

// UpdateProfile applies p and returns the updated user.
func (repository *UserRepository) UpdateProfile(ctx context.Context, id string, update model.UserProfileUpdate) (model.User, error) {
	return one(repository.queries.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
		ID: id, Name: update.Name, SetImage: update.SetImage, Image: update.Image,
	}))
}

// Delete deletes the user; sessions, accounts and app rows cascade.
func (repository *UserRepository) Delete(ctx context.Context, id string) error {
	user, err := repository.queries.DeleteUser(ctx, id)
	if err != nil {
		return err
	}
	if user == 0 {
		return model.ErrNotFound
	}
	return nil
}

// DataSummary lists the data stored for a user.
func (repository *UserRepository) DataSummary(ctx context.Context, id string) (model.UserDataSummary, error) {
	user, err := repository.GetByID(ctx, id)
	if err != nil {
		return model.UserDataSummary{}, err
	}
	sessions, err := repository.queries.CountUserSessions(ctx, id)
	if err != nil {
		return model.UserDataSummary{}, err
	}
	return model.UserDataSummary{User: user, Sessions: sessions}, nil
}

func one(user sqlc.User, err error) (model.User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, model.ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	// Field by field: "user" belongs to packages/auth, whose admin and 2FA
	// columns (role, banned, twoFactorEnabled, ...) are not part of model.User.
	return model.User{
		ID:            user.ID,
		Name:          user.Name,
		Email:         user.Email,
		EmailVerified: user.EmailVerified,
		Image:         user.Image,
		CreatedAt:     user.CreatedAt,
		UpdatedAt:     user.UpdatedAt,
	}, nil
}
