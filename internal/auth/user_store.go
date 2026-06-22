package auth

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
)

// UserRecord is a user document stored in Firestore.
type UserRecord struct {
	Email        string `firestore:"email"`
	PasswordHash string `firestore:"password_hash"`
}

// CreateUser writes a user document to the Firestore users collection.
func CreateUser(ctx context.Context, fs *firestore.Client, userID, email, passwordHash string) error {
	_, err := fs.Collection("users").Doc(userID).Set(ctx, UserRecord{
		Email:        email,
		PasswordHash: passwordHash,
	})
	if err != nil {
		return fmt.Errorf("create user %s: %w", userID, err)
	}
	return nil
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
