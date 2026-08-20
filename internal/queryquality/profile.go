package queryquality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	defaultRetrievalProfileID = "platform-owned-lifestyle-v1"
	maxProfileIDLength        = 128
	maxProfileTerms           = 32
	maxProfileTermLength      = 64
)

var profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// RetrievalProfile is the production-owned, immutable-at-use profile input to retrieval.
// Prompt, model, thresholds, attempts, and selection settings are separate stage config.
type RetrievalProfile struct {
	ID              string          `json:"id"`
	CriterionPolicy CriterionPolicy `json:"criterion_policy"`
}

func DefaultRetrievalProfile() RetrievalProfile {
	return RetrievalProfile{ID: defaultRetrievalProfileID, CriterionPolicy: CriterionPolicy{
		RequiredWhenExplicit: []string{"location", "explicit_exclusion"},
		PreferredByDefault:   []string{"venue_type", "activity", "audience", "setting"},
		GoalsToExpand:        []string{"suitability", "recommendation", "discovery"},
	}}
}

func (p RetrievalProfile) ValidatedCopy() (RetrievalProfile, error) {
	if err := validateProfileID(p.ID); err != nil {
		return RetrievalProfile{}, err
	}
	copy := RetrievalProfile{ID: p.ID, CriterionPolicy: CriterionPolicy{
		RequiredWhenExplicit: append([]string(nil), p.CriterionPolicy.RequiredWhenExplicit...),
		PreferredByDefault:   append([]string(nil), p.CriterionPolicy.PreferredByDefault...),
		GoalsToExpand:        append([]string(nil), p.CriterionPolicy.GoalsToExpand...),
	}}
	seen := map[string]string{}
	for name, values := range map[string][]string{
		"required_when_explicit": copy.CriterionPolicy.RequiredWhenExplicit,
		"preferred_by_default":   copy.CriterionPolicy.PreferredByDefault,
		"goals_to_expand":        copy.CriterionPolicy.GoalsToExpand,
	} {
		if len(values) > maxProfileTerms {
			return RetrievalProfile{}, fmt.Errorf("%s has too many values", name)
		}
		for i, value := range values {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > maxProfileTermLength {
				return RetrievalProfile{}, fmt.Errorf("%s[%d] is malformed", name, i)
			}
			if prior, ok := seen[value]; ok {
				return RetrievalProfile{}, fmt.Errorf("duplicate criterion %q in %s and %s", value, prior, name)
			}
			seen[value] = name
			values[i] = value
		}
	}
	return copy, nil
}

func validateProfileID(id string) error {
	if id == "" || len(id) > maxProfileIDLength || !profileIDPattern.MatchString(id) {
		return errors.New("profile id must be 1-128 lowercase letters, digits, '.', '_' or '-' and start alphanumeric")
	}
	return nil
}

func (p RetrievalProfile) Digest() (string, error) {
	validated, err := p.ValidatedCopy()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(validated)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
