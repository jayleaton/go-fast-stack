package services_test

import (
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

func TestAuditList(test *testing.T) {
	test.Parallel()
	ports := newPorts(test)
	svc := services.NewAuditService(ports.log, services.Policy{RequireTwoFactor: true})
	in := validate.ListAuditLog{Action: "user.ban", TargetID: "u1", Limit: 10, Offset: 20}
	want := services.AuditPage{Entries: []services.AuditEntry{{ID: 7, Action: "user.ban"}}, Total: 21, Limit: 10, Offset: 20}

	ports.log.EXPECT().List(ctx, services.AuditQuery{Action: "user.ban", TargetID: "u1", Limit: 10, Offset: 20}).Return(want, nil)
	if got, err := svc.List(ctx, grace, in); err != nil || len(got.Entries) != 1 || got.Entries[0].ID != 7 || got.Total != 21 {
		test.Fatalf("List = %+v, %v", got, err)
	}

	ports.log.EXPECT().List(ctx, gomock.Any()).Return(services.AuditPage{}, nil)
	if got, err := svc.List(ctx, grace, in); err != nil || got.Entries == nil {
		test.Fatalf("empty page = %#v, %v; want an empty, non-nil list", got, err)
	}

	ports.log.EXPECT().List(ctx, gomock.Any()).Return(services.AuditPage{}, errDB)
	_, err := svc.List(ctx, grace, in)
	wantInternal(test, err, errDB)
}
