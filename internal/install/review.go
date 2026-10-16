package install

import (
	"github.com/rexbrahh/make-better-decisions/internal/policy"
	"github.com/rexbrahh/make-better-decisions/internal/py/json"
)

// An explicit owner selection must replace an invalid prior provider before plan validation.
func previousProviderSpec(host string, selected *string) *json.Object {
	previous := PreviousInstallationSpec(host)
	if previous != nil && selected != nil {
		previous = previous.Copy()
		previous.Set("review_provider", *selected)
	}
	return previous
}

// ConfigureReviewProvider preserves an omitted selection and validates explicit owner changes.
func ConfigureReviewProvider(entry, previousSpec *json.Object, selected *string) error {
	var value any
	var exists bool
	if selected != nil {
		value, exists = *selected, true
	} else if previousSpec != nil {
		value, exists = previousSpec.Get("review_provider")
	}
	if !exists {
		return nil
	}
	name, ok := value.(string)
	if !ok || name == "" {
		return installError("invalid review provider in previous installation")
	}
	provider, err := policy.Provider(name)
	if err != nil {
		return installError(err.Error())
	}
	entry.Value("spec").(*json.Object).Set("review_provider", provider.Name)
	return nil
}
