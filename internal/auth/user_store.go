package auth

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
)

// UserRecord is a user document stored in Firestore.
type UserRecord struct {
	Email         string `firestore:"email"`
	PasswordHash  string `firestore:"password_hash"`
	EmailVerified bool   `firestore:"email_verified"`
	ProjectCount  int    `firestore:"project_count"`
	DefaultProject string `firestore:"default_project"`
}

// CreateUser writes a user document to the Firestore users collection.
func CreateUser(ctx context.Context, fs *firestore.Client, userID, email, passwordHash string) error {
	_, err := fs.Collection("users").Doc(userID).Set(ctx, UserRecord{
		Email:         email,
		PasswordHash:  passwordHash,
		EmailVerified: false,
		ProjectCount:  0,
	})
	if err != nil {
		return fmt.Errorf("create user %s: %w", userID, err)
	}
	return nil
}

// CountProjects returns the number of projects a user has in Firestore.
func CountProjects(ctx context.Context, fs *firestore.Client, userID string) (int, error) {
	iter := fs.Collection("users").Doc(userID).Collection("projects").Documents(ctx)
	defer iter.Stop()
	count := 0
	for {
		_, err := iter.Next()
		if err != nil {
			break
		}
		count++
	}
	return count, nil
}

// GetUser fetches a user record from Firestore by ID.
func GetUser(ctx context.Context, fs *firestore.Client, userID string) (*UserRecord, error) {
	doc, err := fs.Collection("users").Doc(userID).Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user %s: %w", userID, err)
	}
	var u UserRecord
	if err := doc.DataTo(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByEmail iterates the users collection to find a user by email.
func GetUserByEmail(ctx context.Context, fs *firestore.Client, email string) (string, *UserRecord, error) {
	iter := fs.Collection("users").Where("email", "==", email).Limit(1).Documents(ctx)
	defer iter.Stop()
	doc, err := iter.Next()
	if err != nil {
		return "", nil, err
	}
	var u UserRecord
	if err := doc.DataTo(&u); err != nil {
		return "", nil, err
	}
	return doc.Ref.ID, &u, nil
}
