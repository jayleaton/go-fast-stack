package services

//go:generate go tool mockgen -source=analytics.go -destination=mocks/analytics.go -package=mocks

import (
	"context"

	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/model"
)

// Overview is the analytics placeholder's headline numbers (re-exported).
type Overview = model.Overview

// Analytics is the port to whatever answers the analytics page. The template
// ships adminrepo.Stats (counts over the app database); swap in a product
// analytics source (PostHog, a warehouse...) behind the same port.
type Analytics interface {
	Overview(ctx context.Context) (Overview, error)
}

// AnalyticsService serves the analytics page.
type AnalyticsService struct {
	src    Analytics
	policy Policy
}

// NewAnalyticsService returns an AnalyticsService over its port.
func NewAnalyticsService(src Analytics, policy Policy) *AnalyticsService {
	return &AnalyticsService{src: src, policy: policy}
}

// Overview returns the headline numbers.
func (s *AnalyticsService) Overview(ctx context.Context, actor Actor) (Overview, error) {
	if err := s.policy.authorize(actor); err != nil {
		return Overview{}, err
	}
	return s.src.Overview(ctx)
}
