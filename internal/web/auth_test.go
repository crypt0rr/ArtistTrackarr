package web

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestInvitationErrorMessageDoesNotReflectStorageDetails(t *testing.T) {
	storageError := errors.New("UNIQUE constraint failed: users.username secret-token")
	message := invitationErrorMessage(storageError)
	if !strings.Contains(message, "Invitation is invalid") {
		t.Fatalf("generic invitation message=%q", message)
	}
	if strings.Contains(message, storageError.Error()) || strings.Contains(message, "secret-token") {
		t.Fatalf("invitation message reflected storage details: %q", message)
	}
	if got := invitationErrorMessage(sql.ErrNoRows); !strings.Contains(got, "Invitation is invalid") {
		t.Fatalf("expired invitation message=%q", got)
	}
}

func TestInvitationErrorMessageKeepsActionableValidation(t *testing.T) {
	if got := invitationErrorMessage(store.ErrInvalidUsername); !strings.Contains(got, "username") {
		t.Fatalf("invalid username message=%q", got)
	}
	if got := invitationErrorMessage(store.ErrUsernameTaken); !strings.Contains(got, "already in use") {
		t.Fatalf("duplicate username message=%q", got)
	}
}
