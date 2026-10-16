package gates

import (
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
	"strings"
)

func reviewer(ctx Context) policy.ReviewProvider {
	if ctx.Reviewer.Name != "" {
		return ctx.Reviewer
	}
	provider, _ := policy.Provider("jev")
	provider.Model = model
	return provider
}

// Legacy rows used these exact settings before provider selection existed.
func legacyReview(ctx Context) bool {
	provider := reviewer(ctx)
	return provider.Name == "jev" && provider.Model == "jev-1.13.0" &&
		provider.ContextProfile == "jev-1" && reviewContract == "scoped-review-2"
}

func reviewIdentity(ctx Context) *json.Object {
	provider := reviewer(ctx)
	return json.NewObject("provider", provider.Name, "model", provider.Model,
		"context_profile", provider.ContextProfile, "review_contract", reviewContract)
}

func markReviewIdentity(record *json.Object, ctx Context) {
	if !legacyReview(ctx) {
		record.Set("review_identity", reviewIdentity(ctx))
	}
}

func matchesReviewIdentity(entry *json.Object, ctx Context) bool {
	identity, exists := entry.Get("review_identity")
	if !exists {
		return legacyReview(ctx)
	}
	if _, ok := identity.(*json.Object); !ok {
		return false
	}
	same, err := json.Equal(identity, reviewIdentity(ctx))
	return err == nil && same
}

func reviewerName(ctx Context) string {
	if reviewer(ctx).Name == "subq" {
		return "SubQ Snap"
	}
	return "Jev"
}

func reviewPolicy(ctx Context) string {
	if reviewer(ctx).Name == "subq" {
		return strings.ReplaceAll(policy.Policy, "Jev verdict", "review verdict")
	}
	return policy.Policy
}
