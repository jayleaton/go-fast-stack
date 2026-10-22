package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jayleaton/ai-patterns-go/packages/database/model"
	"github.com/jayleaton/ai-patterns-go/packages/database/sqlc"
)

// UserRepository reads and updates the non-credential columns of the Better
// Auth "user" table. packages/auth owns the table and its credentials.
//
// It implements services.UserRepository: the method set is the port the
// service declares, nothing more. Copy this file for a new repository.
type UserRepository struct {
	q *sqlc.Queries
}

// NewUserRepository returns a repository backed by db: a *Pool (live
// DATABASE_URL rotation) or a *pgxpool.Pool.
func NewUserRepository(db sqlc.DBTX) *UserRepository {
	return &UserRepository{q: sqlc.New(db)}
}

// GetByID returns model.ErrNotFound when no user has id.
func (r *UserRepository) GetByID(ctx context.Context, id string) (model.User, error) {
	return one(r.q.GetUserByID(ctx, id))
}

// UpdateProfile applies p and returns the updated user.
func (r *UserRepository) UpdateProfile(ctx context.Context, id string, p model.UserProfileUpdate) (model.User, error) {
	return one(r.q.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
		ID: id, Name: p.Name, SetImage: p.SetImage, Image: p.Image,
	}))
}

// Delete deletes the user; sessions, accounts and app rows cascade.
func (r *UserRepository) Delete(ctx context.Context, id string) error {
	n, err := r.q.DeleteUser(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return model.ErrNotFound
	}
	return nil
}

// DataSummary lists the data stored for a user.
func (r *UserRepository) DataSummary(ctx context.Context, id string) (model.UserDataSummary, error) {
	u, err := r.GetByID(ctx, id)
	if err != nil {
		return model.UserDataSummary{}, err
	}
	sessions, err := r.q.CountUserSessions(ctx, id)
	if err != nil {
		return model.UserDataSummary{}, err
	}
	return model.UserDataSummary{User: u, Sessions: sessions}, nil
}

func one(u sqlc.User, err error) (model.User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, model.ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	// Field by field: "user" belongs to packages/auth, whose admin and 2FA
	// columns (role, banned, twoFactorEnabled, ...) are not part of model.User.
	return model.User{
		ID:            u.ID,
		Name:          u.Name,
		Email:         u.Email,
		EmailVerified: u.EmailVerified,
		Image:         u.Image,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
	}, nil
}
