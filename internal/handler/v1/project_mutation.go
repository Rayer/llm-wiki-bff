package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	errInvalidProjectName = errors.New("name must be 1-64 characters")
	errProjectNotFound    = errors.New("project not found")
	errRenameBodyTooLarge = errors.New("rename request body too large")
)

const maxRenameRequestBytes = 1024

type renameProjectRequest struct {
	Name string `json:"name" binding:"required"`
}

type renameProjectResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type adminRenameProjectResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	UserID string `json:"user_id"`
}

type projectRenameTransactionAdapter interface {
	RenameProject(context.Context, string, string, string, func(string, map[string]interface{}) bool) error
}

type firestoreProjectRenameTransaction struct {
	client *firestore.Client
}

func (a firestoreProjectRenameTransaction) RenameProject(ctx context.Context, userID, projectID, name string, authorize func(string, map[string]interface{}) bool) error {
	docRef := a.client.Collection("projects").Doc(projectDocID(userID, projectID))
	return a.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(docRef)
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return errProjectNotFound
			}
			return err
		}
		if !authorize(docRef.ID, snap.Data()) {
			return errProjectNotFound
		}
		return tx.Update(docRef, []firestore.Update{{Path: "name", Value: name}})
	})
}

// SetProjectRenameFunc injects the project metadata update seam used by
// production-router tests and local callers without a Firestore dependency.
func (h *Handler) SetProjectRenameFunc(fn func(context.Context, string, string, string) error) {
	h.projectRename = fn
}

func normalizeProjectName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 64 {
		return "", errInvalidProjectName
	}
	return value, nil
}

func decodeStrictJSON(body io.Reader, destination any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("invalid JSON: multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeRenameProjectJSON(body io.Reader, destination any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxRenameRequestBytes+1))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errRenameBodyTooLarge
		}
		return err
	}
	if len(data) > maxRenameRequestBytes {
		return errRenameBodyTooLarge
	}
	if !utf8.Valid(data) {
		return errors.New("invalid JSON: malformed UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("invalid JSON: expected object")
	}
	keys := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || keys[key] {
			return errors.New("invalid JSON: duplicate object key")
		}
		keys[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return decodeStrictJSON(bytes.NewReader(data), destination)
}

func logProjectRename(stage, userID, projectID string) {
	log.Printf("project_rename operation=rename stage=%s user_id=%s project_id=%s", stage, userID, projectID)
}
