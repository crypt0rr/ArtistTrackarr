package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/security"
)

func TestPasswordChangeRevokesOutstandingResetLinks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "password-reset-revoke@example.com", "old-hash", "member", "UTC", "reset-revoke")
	if err != nil {
		t.Fatal(err)
	}
	reset, err := s.CreateAuthToken(ctx, "reset", "password-reset-revoke@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := s.CreateAuthToken(ctx, "invite", "new-member@example.com", nil, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePassword(ctx, userID, "changed-hash"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, reset, "stale-reset-hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("password change left reset link usable: %v", err)
	}
	user, err := s.UserByID(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.PasswordHash != "changed-hash" {
		t.Fatalf("stale reset link changed password hash to %q, want the password-change hash", user.PasswordHash)
	}
	if email, linkedUser, err := s.ConsumeAuthToken(ctx, invite, "invite"); err != nil || email != "new-member@example.com" || linkedUser != nil {
		t.Fatalf("password change altered invitation: email=%q linkedUser=%v err=%v", email, linkedUser, err)
	}
}

func TestNewResetLinkSupersedesPrevious(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "reset-supersede@example.com", "old-hash", "member", "UTC", "reset-supersede")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateAuthToken(ctx, "reset", "reset-supersede@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.CreateAuthToken(ctx, "reset", "reset-supersede@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, first, "stale-hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("previous reset link remained usable: %v", err)
	}

	// Simulate a still-unused sibling created before reset-link replacement was
	// introduced. Redeeming the latest link must invalidate that legacy token too.
	legacySibling := "legacy-sibling-reset-token"
	now := time.Now().UTC()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash,kind,email,user_id,expires_at,created_by,created_at)
		VALUES(?,'reset',?,?,?,?,?)`, security.Digest(legacySibling), "reset-supersede@example.com", userID,
		timeText(now.Add(time.Hour)), userID, timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, latest, "latest-hash"); err != nil {
		t.Fatalf("latest reset link failed: %v", err)
	}
	if err := s.ResetPasswordWithToken(ctx, legacySibling, "sibling-hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("redeeming the latest reset link left its sibling usable: %v", err)
	}
}

func TestFailedResetLinkIssuancePreservesPreviousLink(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "reset-issue-atomic@example.com", "old-hash", "member", "UTC", "reset-issue-atomic")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := s.CreateAuthToken(ctx, "reset", "reset-issue-atomic@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TRIGGER reject_reset_token BEFORE INSERT ON auth_tokens
		WHEN NEW.kind='reset' BEGIN SELECT RAISE(ABORT, 'injected reset token failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthToken(ctx, "reset", "reset-issue-atomic@example.com", &userID, userID, time.Hour); err == nil {
		t.Fatal("injected reset token insertion unexpectedly succeeded")
	}
	if _, err := s.DB.ExecContext(ctx, `DROP TRIGGER reject_reset_token`); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, previous, "previous-link-hash"); err != nil {
		t.Fatalf("failed reset-link issuance consumed the previous link: %v", err)
	}
}
