package policy

import (
	"fmt"
	"time"

	"github.com/rexbrahh/make-better-decisions/internal/py/pathlib"
)

const (
	SubQModel = "subq-snap-preview"
	SubQURL   = "https://api.subq.ai/v1/systemone"
)

type EvidenceProfile struct {
	BudgetChars int
	TextChars   int
}

type ReviewProvider struct {
	Name           string
	Model          string
	ContextProfile string
	Timeout        time.Duration
	Evidence       EvidenceProfile
}

// ReviewProviderError is invalid owner configuration, not a service availability failure.
type ReviewProviderError struct{ Message string }

func (e *ReviewProviderError) Error() string { return e.Message }

// Provider returns fixed settings; only the signed installation chooses the name.
func Provider(name string) (ReviewProvider, error) {
	switch name {
	case "", "jev":
		return ReviewProvider{"jev", Model, "jev-1", time.Duration(TimeoutS * float64(time.Second)),
			EvidenceProfile{EvidenceChars, ClipChars}}, nil
	case "subq":
		return ReviewProvider{"subq", SubQModel, "snap-2", 10 * time.Second,
			EvidenceProfile{800_000, 160_000}}, nil
	default:
		return ReviewProvider{}, &ReviewProviderError{Message: fmt.Sprintf("unknown review provider %q", name)}
	}
}

func SubQKeyFile() (string, error) {
	home, err := pathlib.Home()
	if err != nil {
		return "", err
	}
	return pathlib.Join(home, ".config/sops-nix/secrets/subq_api_key"), nil
}
