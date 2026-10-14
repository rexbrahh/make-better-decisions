package ledger

import "github.com/rexbrahh/make-better-decisions/internal/py/json"

// The owner policy functions of policy.py. They read the ledger, so they live here to avoid an import
// cycle between packages policy and ledger.

// InstallationPolicy returns the latest verified owner policy for this host, or an empty dict.
//
// py: agent_decisions/policy.py installation_policy
func InstallationPolicy(host string) (*json.Object, error) {
	rows, err := Entries("installation", nil)
	if err != nil {
		return nil, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		entry := rows[i]
		spec, isDict := entry.Value("spec").(*json.Object)
		if entry.Value("gate") == "installation" && isDict && spec.Value("host") == host {
			return spec, nil
		}
	}
	return &json.Object{}, nil
}

// ReviewMode returns "availability" or "required". Only the protected, signed installation policy can
// permit unchecked actions.
//
// py: agent_decisions/policy.py review_mode
func ReviewMode(host string) (string, error) {
	spec, err := InstallationPolicy(host)
	if err != nil {
		return "", err
	}
	if spec.Value("review_mode") == "availability" {
		return "availability", nil
	}
	return "required", nil
}

// SessionReviewException reports whether the owner policy of host exempts sessionID from review.
//
// py: agent_decisions/policy.py session_review_exception
func SessionReviewException(host, sessionID string) (bool, error) {
	spec, err := InstallationPolicy(host)
	if err != nil {
		return false, err
	}
	sessions, isList := spec.Value("session_review_exceptions").([]any)
	if !isList {
		return false, nil
	}
	found := false
	for _, session := range sessions {
		name, isStr := session.(string)
		if !isStr {
			return false, nil
		}
		found = found || name == sessionID
	}
	return found, nil
}

// FailOpen reports whether host runs in availability mode.
//
// py: agent_decisions/policy.py fail_open
func FailOpen(host string) (bool, error) {
	mode, err := ReviewMode(host)
	return mode == "availability", err
}
