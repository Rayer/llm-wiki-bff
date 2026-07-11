package syssettings

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	settingsCollection = "system"
	settingsDocID      = "settings"
)

// Settings is the public system settings payload.
type Settings struct {
	RegistrationEnabled bool `json:"registration_enabled"`
}

// RegistrationGate reports whether self-serve registration is allowed.
type RegistrationGate interface {
	IsRegistrationEnabled(ctx context.Context) (bool, error)
	GetSettings(ctx context.Context) (Settings, error)
	SetRegistrationEnabled(ctx context.Context, enabled bool) (Settings, error)
}

// Store resolves and persists registration settings in Firestore with env fallback.
type Store struct {
	fs       *firestore.Client
	envValue *bool
}

// NewStore creates a settings store. envValue is nil when REGISTRATION_ENABLED is unset.
func NewStore(fs *firestore.Client, envValue *bool) *Store {
	return &Store{fs: fs, envValue: envValue}
}

func (s *Store) settingsRef() *firestore.DocumentRef {
	return s.fs.Collection(settingsCollection).Doc(settingsDocID)
}

func (s *Store) loadDoc(ctx context.Context) (bool, bool, error) {
	doc, err := s.settingsRef().Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return false, false, nil
		}
		return false, false, fmt.Errorf("get system settings: %w", err)
	}
	data := doc.Data()
	enabled, ok := data["registration_enabled"].(bool)
	if !ok {
		enabled = false
	}
	return true, enabled, nil
}

// IsRegistrationEnabled implements RegistrationGate.
func (s *Store) IsRegistrationEnabled(ctx context.Context) (bool, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return false, err
	}
	return settings.RegistrationEnabled, nil
}

// GetSettings returns the resolved registration_enabled value.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	if s.fs == nil {
		return Settings{RegistrationEnabled: Resolve(false, false, s.envValue)}, nil
	}
	docExists, firestoreValue, err := s.loadDoc(ctx)
	if err != nil {
		return Settings{}, err
	}
	return Settings{RegistrationEnabled: Resolve(docExists, firestoreValue, s.envValue)}, nil
}

// SetRegistrationEnabled persists registration_enabled to Firestore system/settings.
func (s *Store) SetRegistrationEnabled(ctx context.Context, enabled bool) (Settings, error) {
	if s.fs == nil {
		return Settings{}, fmt.Errorf("Firestore client is not configured")
	}
	_, err := s.settingsRef().Set(ctx, map[string]interface{}{
		"registration_enabled": enabled,
	}, firestore.MergeAll)
	if err != nil {
		return Settings{}, fmt.Errorf("set system settings: %w", err)
	}
	return Settings{RegistrationEnabled: enabled}, nil
}

// FakeStore is an in-memory RegistrationGate for tests.
type FakeStore struct {
	Enabled      bool
	Err          error
	Persisted    *bool
	GetCalls     int
	SetCalls     int
	LastSetValue bool
}

func (f *FakeStore) IsRegistrationEnabled(ctx context.Context) (bool, error) {
	f.GetCalls++
	if f.Err != nil {
		return false, f.Err
	}
	if f.Persisted != nil {
		return *f.Persisted, nil
	}
	return f.Enabled, nil
}

func (f *FakeStore) GetSettings(ctx context.Context) (Settings, error) {
	enabled, err := f.IsRegistrationEnabled(ctx)
	if err != nil {
		return Settings{}, err
	}
	return Settings{RegistrationEnabled: enabled}, nil
}

func (f *FakeStore) SetRegistrationEnabled(ctx context.Context, enabled bool) (Settings, error) {
	f.SetCalls++
	f.LastSetValue = enabled
	if f.Err != nil {
		return Settings{}, f.Err
	}
	f.Persisted = &enabled
	f.Enabled = enabled
	return Settings{RegistrationEnabled: enabled}, nil
}