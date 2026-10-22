// Package servicetest provides in-memory repositories for service and
// transport tests. Repository behaviour itself is tested against Postgres
// in packages/database.
package servicetest

import (
	"context"
	"sync"
	"time"

	"github.com/jayleaton/ai-patterns-go/packages/database/model"
)

// Users is an in-memory services.UserRepository.
type Users struct {
	mu    sync.Mutex
	users map[string]model.User
}

// NewUsers returns a repository seeded with users.
func NewUsers(users ...model.User) *Users {
	r := &Users{users: map[string]model.User{}}
	for _, u := range users {
		r.users[u.ID] = u
	}
	return r
}

func (r *Users) GetUserByID(_ context.Context, id string) (model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return model.User{}, model.ErrNotFound
	}
	return u, nil
}

func (r *Users) UpdateUserProfile(_ context.Context, id string, p model.UserProfileUpdate) (model.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return model.User{}, model.ErrNotFound
	}
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.SetImage {
		u.Image = p.Image
	}
	u.UpdatedAt = time.Now()
	r.users[id] = u
	return u, nil
}

func (r *Users) DeleteUser(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.users[id]; !ok {
		return model.ErrNotFound
	}
	delete(r.users, id)
	return nil
}

func (r *Users) GetUserDataSummary(ctx context.Context, id string) (model.UserDataSummary, error) {
	u, err := r.GetUserByID(ctx, id)
	return model.UserDataSummary{User: u}, err
}
