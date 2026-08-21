package queryconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

var ErrBindingMismatch = errors.New("query config binding mismatch")
var ErrInvalidGenerationIdentity = errors.New("invalid query generation identity")

type BindingMismatchError struct{}

func (BindingMismatchError) Error() string { return ErrBindingMismatch.Error() }
func (BindingMismatchError) Unwrap() error { return ErrBindingMismatch }

type GenerationIdentity struct {
	ProjectID      string
	GenerationID   string
	ConceptsDigest string
}

type EffectiveConfig struct {
	SchemaVersion              int
	ConfigRevision             string
	ConfigDigest               string
	QueryServiceImplementation string
	Profile                    queryquality.RetrievalProfile
	ProfileDigest              string
	PromptID                   string
	PromptDigest               string
	Options                    queryquality.Options
	ExpansionImplementation    string
	ExpansionProvider          string
	ExpansionModel             string
	ExpansionReasoning         string
	ExpansionTemperature       float64
	SynthesisImplementation    string
	SynthesisProvider          string
	SynthesisModel             string
	SynthesisReasoning         string
	SynthesisTemperature       float64
	BindingSource              string
	ExactBinding               bool
	InputGenerationIdentity    GenerationIdentity
	EffectiveConfigDigest      string
}

type Resolver struct {
	config        Config
	profiles      map[string]Profile
	bindings      map[string][]ProjectBinding
	defaultConfig EffectiveConfig
	compositions  []EffectiveConfig
}

func NewResolver(config Config) (*Resolver, error) {
	if err := ValidateSealed(config); err != nil {
		return nil, fmt.Errorf("invalid sealed query config: %w", err)
	}
	sealed, err := Normalize(config)
	if err != nil {
		return nil, err
	}
	resolver := &Resolver{
		config:   sealed,
		profiles: make(map[string]Profile, len(sealed.Profiles)),
		bindings: make(map[string][]ProjectBinding),
	}
	for _, profile := range sealed.Profiles {
		resolver.profiles[profile.ID] = cloneProfile(profile)
	}
	for _, binding := range sealed.ProjectBindings {
		copy := cloneBinding(binding)
		resolver.bindings[binding.ProjectID] = append(resolver.bindings[binding.ProjectID], copy)
	}
	resolver.defaultConfig, err = resolver.effective(GenerationIdentity{}, false, "")
	if err != nil {
		return nil, err
	}
	resolver.compositions = append(resolver.compositions, resolver.defaultConfig)
	for _, binding := range sealed.ProjectBindings {
		identity := GenerationIdentity{ProjectID: binding.ProjectID, GenerationID: binding.GenerationID, ConceptsDigest: binding.ConceptsDigest}
		effective, err := resolver.effective(identity, true, binding.ProjectID)
		if err != nil {
			return nil, err
		}
		resolver.compositions = append(resolver.compositions, effective)
	}
	return resolver, nil
}

func (r *Resolver) Resolve(identity GenerationIdentity) (EffectiveConfig, error) {
	if r == nil || identity.ProjectID == "" {
		return EffectiveConfig{}, ErrInvalidGenerationIdentity
	}
	bindings := r.bindings[identity.ProjectID]
	if len(bindings) > 0 {
		for _, binding := range bindings {
			if binding.GenerationID == identity.GenerationID && binding.ConceptsDigest == identity.ConceptsDigest {
				return cloneEffectiveConfig(r.effectiveForBinding(binding, identity)), nil
			}
		}
		return EffectiveConfig{}, BindingMismatchError{}
	}
	if identity.GenerationID == "" || (identity.ConceptsDigest == "" && identity.GenerationID != "legacy") {
		return EffectiveConfig{}, ErrInvalidGenerationIdentity
	}
	if identity.ConceptsDigest != "" && validateDigest("concepts_digest", identity.ConceptsDigest) != nil {
		return EffectiveConfig{}, ErrInvalidGenerationIdentity
	}
	return cloneEffectiveConfig(r.makeDefault(identity)), nil
}

func (r *Resolver) makeDefault(identity GenerationIdentity) EffectiveConfig {
	config := r.defaultConfig
	config.InputGenerationIdentity = identity
	config.EffectiveConfigDigest = digestEffective(config)
	return config
}

// EffectiveConfigs returns a defensive copy of the default and each pinned
// binding composition for startup prebuilding.
func (r *Resolver) EffectiveConfigs() []EffectiveConfig {
	if r == nil {
		return nil
	}
	result := make([]EffectiveConfig, 0, len(r.compositions))
	for _, config := range r.compositions {
		result = append(result, cloneEffectiveConfig(config))
	}
	return result
}

func (r *Resolver) effective(identity GenerationIdentity, exact bool, projectID string) (EffectiveConfig, error) {
	profile, err := lookupProfile(r.profiles, r.config.Stages.QueryExpander.DefaultProfileID, r.config.Stages.QueryExpander.DefaultProfileDigest)
	if err != nil {
		return EffectiveConfig{}, err
	}
	promptID := r.config.Stages.QueryExpander.DefaultPromptID
	promptDigest := r.config.Stages.QueryExpander.DefaultPromptDigest
	source := SourceLegacyCompatibility
	if exact {
		bindings := r.bindings[projectID]
		for _, binding := range bindings {
			if binding.GenerationID == identity.GenerationID && binding.ConceptsDigest == identity.ConceptsDigest {
				return r.effectiveForBinding(binding, identity), nil
			}
		}
		return EffectiveConfig{}, ErrBindingMismatch
	}
	return r.makeEffective(identity, profile, promptID, promptDigest, source, false), nil
}

func (r *Resolver) effectiveForBinding(binding ProjectBinding, identity GenerationIdentity) EffectiveConfig {
	profile := r.profiles[binding.ProfileID]
	return r.makeEffective(identity, queryquality.RetrievalProfile{ID: profile.ID, CriterionPolicy: profile.CriterionPolicy}, binding.PromptID, binding.PromptDigest, binding.Source, true)
}

func (r *Resolver) makeEffective(identity GenerationIdentity, profile queryquality.RetrievalProfile, promptID, promptDigest, source string, exact bool) EffectiveConfig {
	stages := r.config.Stages
	options := queryquality.Options{
		SelectionLimit: stages.ResultSelector.Limit, ExplorationSlots: stages.ResultSelector.ExplorationSlots,
		EvidenceThreshold: stages.CandidateMatcher.EvidenceThreshold, EvidenceThresholdSet: true,
		KeywordsPerAttempt: stages.QueryExpander.KeywordsPerAttempt, ExpansionAttempts: stages.QueryExpander.Attempts,
		RareDocumentFrequency: stages.CandidateMatcher.RareKeywordMaxDocumentFrequency,
	}
	profile, _ = profile.ValidatedCopy()
	profileDigest, _ := profile.Digest()
	effective := EffectiveConfig{
		SchemaVersion: r.config.SchemaVersion, ConfigRevision: r.config.ConfigRevision, ConfigDigest: r.config.ConfigDigest,
		QueryServiceImplementation: r.config.QueryServiceImplementation, Profile: profile, ProfileDigest: profileDigest,
		PromptID: promptID, PromptDigest: promptDigest, Options: options,
		ExpansionImplementation: stages.QueryExpander.Implementation, ExpansionProvider: stages.QueryExpander.Provider, ExpansionModel: stages.QueryExpander.Model,
		ExpansionReasoning: stages.QueryExpander.Reasoning, ExpansionTemperature: stages.QueryExpander.Temperature,
		SynthesisImplementation: stages.AnswerSynthesizer.Implementation, SynthesisProvider: stages.AnswerSynthesizer.Provider, SynthesisModel: stages.AnswerSynthesizer.Model,
		SynthesisReasoning: stages.AnswerSynthesizer.Reasoning, SynthesisTemperature: stages.AnswerSynthesizer.Temperature,
		BindingSource: source, ExactBinding: exact, InputGenerationIdentity: identity,
	}
	effective.EffectiveConfigDigest = digestEffective(effective)
	return effective
}

func lookupProfile(profiles map[string]Profile, id, digest string) (queryquality.RetrievalProfile, error) {
	profile, ok := profiles[id]
	if !ok || profile.ProfileDigest != digest {
		return queryquality.RetrievalProfile{}, errors.New("query config profile reference mismatch")
	}
	return queryquality.RetrievalProfile{ID: profile.ID, CriterionPolicy: profile.CriterionPolicy}.ValidatedCopy()
}

func (e EffectiveConfig) RuntimeConfigIdentity() query.RuntimeConfigIdentity {
	return query.RuntimeConfigIdentity{
		SchemaVersion: e.SchemaVersion, ConfigRevision: e.ConfigRevision, ConfigDigest: e.ConfigDigest,
		EffectiveConfigDigest: e.EffectiveConfigDigest, QueryServiceImplementation: e.QueryServiceImplementation,
		ProfileID: e.Profile.ID, ProfileDigest: e.ProfileDigest, PromptID: e.PromptID, PromptDigest: e.PromptDigest,
		BindingSource: e.BindingSource, ExactBinding: e.ExactBinding, GenerationID: e.InputGenerationIdentity.GenerationID,
		ConceptsDigest: e.InputGenerationIdentity.ConceptsDigest, ExpansionProvider: e.ExpansionProvider, ExpansionImplementation: e.ExpansionImplementation,
		ExpansionModel: e.ExpansionModel, ExpansionReasoning: e.ExpansionReasoning, ExpansionTemperature: e.ExpansionTemperature,
		SynthesisImplementation: e.SynthesisImplementation, SynthesisProvider: e.SynthesisProvider, SynthesisModel: e.SynthesisModel, SynthesisReasoning: e.SynthesisReasoning,
		SynthesisTemperature: e.SynthesisTemperature, SelectionLimit: e.Options.SelectionLimit, ExplorationSlots: e.Options.ExplorationSlots,
		EvidenceThreshold: e.Options.EvidenceThreshold, KeywordsPerAttempt: e.Options.KeywordsPerAttempt,
		ExpansionAttempts: e.Options.ExpansionAttempts, RareDocumentFrequency: e.Options.RareDocumentFrequency,
	}
}

// EffectiveConfigDigestWithoutGeneration identifies only the immutable
// service composition; pinned corpus identity is tracked separately.
func (e EffectiveConfig) EffectiveConfigDigestWithoutGeneration() string {
	e.InputGenerationIdentity = GenerationIdentity{}
	e.BindingSource = ""
	e.ExactBinding = false
	return digestEffective(e)
}

type effectiveDigestInput struct {
	SchemaVersion                                                                  int `json:"schema_version"`
	ConfigRevision, ConfigDigest, QueryServiceImplementation                       string
	Profile                                                                        queryquality.RetrievalProfile
	ProfileDigest, PromptID, PromptDigest                                          string
	Options                                                                        struct{ SelectionLimit, ExplorationSlots, EvidenceThreshold, KeywordsPerAttempt, ExpansionAttempts, RareDocumentFrequency int } `json:"options"`
	ExpansionImplementation, ExpansionProvider, ExpansionModel, ExpansionReasoning string
	ExpansionTemperature                                                           float64
	SynthesisImplementation, SynthesisProvider, SynthesisModel, SynthesisReasoning string
	SynthesisTemperature                                                           float64
	BindingSource                                                                  string
	ExactBinding                                                                   bool
	InputGenerationIdentity                                                        GenerationIdentity
}

func digestEffective(config EffectiveConfig) string {
	input := effectiveDigestInput{
		SchemaVersion: config.SchemaVersion, ConfigRevision: config.ConfigRevision, ConfigDigest: config.ConfigDigest,
		QueryServiceImplementation: config.QueryServiceImplementation, Profile: config.Profile, ProfileDigest: config.ProfileDigest,
		PromptID: config.PromptID, PromptDigest: config.PromptDigest, ExpansionImplementation: config.ExpansionImplementation,
		ExpansionProvider: config.ExpansionProvider, ExpansionModel: config.ExpansionModel, ExpansionReasoning: config.ExpansionReasoning, ExpansionTemperature: config.ExpansionTemperature,
		SynthesisImplementation: config.SynthesisImplementation, SynthesisProvider: config.SynthesisProvider, SynthesisModel: config.SynthesisModel, SynthesisReasoning: config.SynthesisReasoning,
		SynthesisTemperature: config.SynthesisTemperature, BindingSource: config.BindingSource, ExactBinding: config.ExactBinding,
		InputGenerationIdentity: config.InputGenerationIdentity,
	}
	input.Options.SelectionLimit = config.Options.SelectionLimit
	input.Options.ExplorationSlots = config.Options.ExplorationSlots
	input.Options.EvidenceThreshold = config.Options.EvidenceThreshold
	input.Options.KeywordsPerAttempt = config.Options.KeywordsPerAttempt
	input.Options.ExpansionAttempts = config.Options.ExpansionAttempts
	input.Options.RareDocumentFrequency = config.Options.RareDocumentFrequency
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneProfile(profile Profile) Profile {
	profile.CriterionPolicy.RequiredWhenExplicit = append([]string(nil), profile.CriterionPolicy.RequiredWhenExplicit...)
	profile.CriterionPolicy.PreferredByDefault = append([]string(nil), profile.CriterionPolicy.PreferredByDefault...)
	profile.CriterionPolicy.GoalsToExpand = append([]string(nil), profile.CriterionPolicy.GoalsToExpand...)
	return profile
}

func cloneBinding(binding ProjectBinding) ProjectBinding { return binding }

func cloneEffectiveConfig(config EffectiveConfig) EffectiveConfig {
	config.Profile, _ = config.Profile.ValidatedCopy()
	if config.Options.Seed != nil {
		seed := *config.Options.Seed
		config.Options.Seed = &seed
	}
	return config
}
