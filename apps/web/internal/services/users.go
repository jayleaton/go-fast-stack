package services

import (
	"context"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/packages/database/model"
)

// User and UserDataSummary are the repository models, re-exported so
// transports never import packages/database.
type (
	User            = model.User
	UserDataSummary = model.UserDataSummary
)

// UserRepository is the data access UserService needs
// (database.UserRepository in production).
type UserRepository interface {
	GetUserByID(ctx context.Context, id string) (model.User, error)
	UpdateUserProfile(ctx context.Context, id string, u model.UserProfileUpdate) (model.User, error)
	DeleteUser(ctx context.Context, id string) error
	GetUserDataSummary(ctx context.Context, id string) (model.UserDataSummary, error)
}

// UserService manages the signed-in user's own profile and data.
type UserService struct {
	repo UserRepository
}

// NewUserService returns a UserService over repo.
func NewUserService(repo UserRepository) *UserService {
	return &UserService{repo: repo}
}

// GetUser returns the actor's user row.
func (s *UserService) GetUser(ctx context.Context, actor Actor) (User, error) {
	u, err := s.repo.GetUserByID(ctx, actor.UserID)
	return u, notFound(err, "User not found")
}

// UpdateUserSettings updates userID's profile. Users may only update themselves.
func (s *UserService) UpdateUserSettings(ctx context.Context, actor Actor, userID string, in validate.UpdateUserSettings) (User, error) {
	if userID != actor.UserID {
		return User{}, fail(KindForbidden, "Cannot update another user's settings")
	}
	u, err := s.repo.UpdateUserProfile(ctx, userID, model.UserProfileUpdate{
		Name: in.Name, SetImage: in.Image.Set, Image: in.Image.Value,
	})
	return u, notFound(err, "User not found")
}

// GetUserDataSummary lists what the app stores for the actor.
func (s *UserService) GetUserDataSummary(ctx context.Context, actor Actor) (UserDataSummary, error) {
	sum, err := s.repo.GetUserDataSummary(ctx, actor.UserID)
	return sum, notFound(err, "User not found")
}

// DeleteAllUserData deletes the actor's account; sessions and app rows cascade.
func (s *UserService) DeleteAllUserData(ctx context.Context, actor Actor) error {
	return notFound(s.repo.DeleteUser(ctx, actor.UserID), "User not found")
}
