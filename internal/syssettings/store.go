package syssettings

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	settingsCollection = "system"
	settingsDocID      = "settings"
	maxMarkdownBytes   = 32 << 10
)

// Settings is the public system settings payload.
type Settings struct {
	RegistrationEnabled  bool   `json:"registration_enabled"`
	AnnouncementMarkdown string `json:"announcement_markdown"`
}

// PublicSettings is the deliberately metadata-free anonymous response.
type PublicSettings struct {
	RegistrationEnabled  bool    `json:"registration_enabled"`
	AnnouncementMarkdown string  `json:"announcement_markdown"`
	AnnouncementDigest   *string `json:"announcement_digest"`
}

// RegistrationGate reports whether self-serve registration is allowed.
type RegistrationGate interface {
	IsRegistrationEnabled(ctx context.Context) (bool, error)
	GetSettings(ctx context.Context) (Settings, error)
	SetRegistrationEnabled(ctx context.Context, enabled bool) (Settings, error)
	PublishAnnouncement(ctx context.Context, markdown string) (Settings, error)
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
	settings := Settings{RegistrationEnabled: Resolve(docExists, firestoreValue, s.envValue)}
	if docExists {
		data, err := s.settingsRef().Get(ctx)
		if err != nil {
			return Settings{}, fmt.Errorf("get system settings: %w", err)
		}
		settings.AnnouncementMarkdown, _ = data.Data()["announcement_published_markdown"].(string)
	}
	return settings, nil
}

func validateMarkdown(markdown string) error {
	if !utf8.ValidString(markdown) {
		return fmt.Errorf("announcement markdown is not valid UTF-8")
	}
	if len([]byte(markdown)) > maxMarkdownBytes {
		return fmt.Errorf("announcement markdown exceeds %d bytes", maxMarkdownBytes)
	}
	return nil
}

func (s *Store) PublishAnnouncement(ctx context.Context, markdown string) (Settings, error) {
	if err := validateMarkdown(markdown); err != nil {
		return Settings{}, err
	}
	if s.fs == nil {
		return Settings{}, fmt.Errorf("Firestore client is not configured")
	}
	err := s.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		return tx.Set(s.settingsRef(), map[string]interface{}{"announcement_published_markdown": markdown}, firestore.MergeAll)
	})
	if err != nil {
		return Settings{}, fmt.Errorf("publish announcement: %w", err)
	}
	return s.GetSettings(ctx)
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
	return s.GetSettings(ctx)
}

// FakeStore is an in-memory RegistrationGate for tests.
type FakeStore struct {
	Enabled      bool
	Err          error
	Persisted    *bool
	GetCalls     int
	SetCalls     int
	LastSetValue bool
	Published    string
	PublishErr   error
	mu           sync.RWMutex
}

func (f *FakeStore) IsRegistrationEnabled(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Settings{RegistrationEnabled: enabled, AnnouncementMarkdown: f.Published}, nil
}

func (f *FakeStore) PublishAnnouncement(ctx context.Context, markdown string) (Settings, error) {
	if err := validateMarkdown(markdown); err != nil {
		return Settings{}, err
	}
	if f.PublishErr != nil {
		return Settings{}, f.PublishErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Published = markdown
	return Settings{RegistrationEnabled: f.Enabled, AnnouncementMarkdown: f.Published}, nil
}

func (f *FakeStore) SetRegistrationEnabled(ctx context.Context, enabled bool) (Settings, error) {
	f.SetCalls++
	f.LastSetValue = enabled
	if f.Err != nil {
		return Settings{}, f.Err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Persisted = &enabled
	f.Enabled = enabled
	return Settings{RegistrationEnabled: enabled, AnnouncementMarkdown: f.Published}, nil
}
