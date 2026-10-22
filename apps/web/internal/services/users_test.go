package services_test

import (
	"context"
	"testing"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services/servicetest"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
)

func ptr[T any](v T) *T { return &v }

func TestUpdateUserSettings(t *testing.T) {
	ctx := context.Background()
	repo := servicetest.NewUsers(services.User{ID: "u1", Name: "Ada", Email: "ada@example.com", Image: ptr("https://x/a.png")})
	svc := services.NewUserService(repo)
	ada := services.Actor{UserID: "u1"}

	_, err := svc.UpdateUserSettings(ctx, ada, "u2", validate.UpdateUserSettings{Name: ptr("Eve")})
	if services.KindOf(err) != services.KindForbidden {
		t.Fatalf("other user: err = %v, want forbidden", err)
	}

	u, err := svc.UpdateUserSettings(ctx, ada, "u1", validate.UpdateUserSettings{Name: ptr("Ada L")})
	if err != nil || u.Name != "Ada L" || u.Image == nil {
		t.Fatalf("rename = %+v, %v", u, err)
	}

	u, err = svc.UpdateUserSettings(ctx, ada, "u1", validate.UpdateUserSettings{Image: validate.Nullable[string]{Set: true}})
	if err != nil || u.Image != nil {
		t.Fatalf("clear image = %+v, %v", u, err)
	}

	ghost := services.Actor{UserID: "gone"}
	if _, err := svc.UpdateUserSettings(ctx, ghost, "gone", validate.UpdateUserSettings{}); services.KindOf(err) != services.KindNotFound {
		t.Fatalf("missing user: err = %v, want not found", err)
	}
}

func TestDeleteAllUserData(t *testing.T) {
	ctx := context.Background()
	repo := servicetest.NewUsers(services.User{ID: "u1"})
	svc := services.NewUserService(repo)
	ada := services.Actor{UserID: "u1"}

	if sum, err := svc.GetUserDataSummary(ctx, ada); err != nil || sum.User.ID != "u1" {
		t.Fatalf("summary = %+v, %v", sum, err)
	}
	if err := svc.DeleteAllUserData(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetUser(ctx, ada); services.KindOf(err) != services.KindNotFound {
		t.Fatalf("after delete: err = %v, want not found", err)
	}
	if err := svc.DeleteAllUserData(ctx, ada); services.KindOf(err) != services.KindNotFound {
		t.Fatalf("second delete: err = %v, want not found", err)
	}
}
