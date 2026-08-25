package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestAuthSessionTokenAndLoginLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "auth-lifecycle@example.com", "old-hash", "member", "UTC", "auth-lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if count, err := s.UserCount(ctx); err != nil || count != 1 {
		t.Fatalf("user count=%d err=%v", count, err)
	}
	raw, err := s.CreateSession(ctx, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if session, err := s.Session(ctx, raw); err != nil || session.User.ID != userID {
		t.Fatalf("session=%#v err=%v", session, err)
	}
	if err := s.UpdatePassword(ctx, userID, "new-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, raw); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("password update left session usable: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.UpdatePassword(canceled, userID, "should-not-apply"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled password update error=%v", err)
	}
	if err := s.UpdatePassword(ctx, 999999, "missing-user"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing password user error=%v, want sql.ErrNoRows", err)
	}
	updatedUser, err := s.UserByID(ctx, userID)
	if err != nil || updatedUser.PasswordHash != "new-hash" {
		t.Fatalf("canceled password update changed hash=%q err=%v", updatedUser.PasswordHash, err)
	}
	newRaw, err := s.CreateSession(ctx, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, newRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, newRaw); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted session lookup error=%v", err)
	}

	invite, err := s.CreateAuthToken(ctx, "invite", "new-member@example.com", nil, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	email, invitedUser, err := s.ConsumeAuthToken(ctx, invite, "invite")
	if err != nil || email != "new-member@example.com" || invitedUser != nil {
		t.Fatalf("consumed invite email=%q user=%v err=%v", email, invitedUser, err)
	}
	if _, _, err := s.ConsumeAuthToken(ctx, invite, "invite"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invite was consumed twice: %v", err)
	}
	reset, err := s.CreateAuthToken(ctx, "reset", "auth-lifecycle@example.com", &userID, userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPasswordWithToken(ctx, reset, "reset-hash"); err != nil {
		t.Fatal(err)
	}
	user, err := s.UserByID(ctx, userID)
	if err != nil || user.PasswordHash != "reset-hash" {
		t.Fatalf("reset user=%#v err=%v", user, err)
	}
	if err := s.ResetPasswordWithToken(ctx, reset, "again"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reset token was consumed twice: %v", err)
	}

	allowed, err := s.LoginAllowed(ctx, "auth-lifecycle@example.com")
	if err != nil || !allowed {
		t.Fatalf("initial login allowance=%v err=%v", allowed, err)
	}
	for range 5 {
		if err := s.RecordLoginFailure(ctx, "auth-lifecycle@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	allowed, err = s.LoginAllowed(ctx, "auth-lifecycle@example.com")
	if err != nil || allowed {
		t.Fatalf("login remained allowed after failures: %v err=%v", allowed, err)
	}
	s.ClearLoginFailures(ctx, "auth-lifecycle@example.com")
	allowed, err = s.LoginAllowed(ctx, "auth-lifecycle@example.com")
	if err != nil || !allowed {
		t.Fatalf("login remained blocked after clear: %v err=%v", allowed, err)
	}
}

func TestUserValidationAndCaseInsensitiveUsernameOwnership(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.CreateUser(ctx, "missing-at", "hash", "member", "UTC", "valid-user"); err == nil {
		t.Fatal("invalid email was accepted")
	}
	if _, err := s.CreateUser(ctx, "timezone@example.com", "hash", "member", "Not/AZone", "valid-user"); err == nil {
		t.Fatal("invalid timezone was accepted")
	}
	if _, err := s.CreateUser(ctx, "empty-timezone@example.com", "hash", "member", "  ", "empty-timezone"); err == nil {
		t.Fatal("empty timezone was accepted")
	}
	for _, username := range []string{"ab", "this username is invalid", ""}[:2] {
		if _, err := s.CreateUser(ctx, "invalid-"+username+"@example.com", "hash", "member", "UTC", username); !errors.Is(err, ErrInvalidUsername) {
			t.Fatalf("username %q error=%v, want ErrInvalidUsername", username, err)
		}
	}
	firstID, err := s.CreateUser(ctx, "first@example.com", "hash", "member", "UTC", "CaseUser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "second@example.com", "hash", "member", "UTC", "caseuser"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("case-insensitive duplicate error=%v", err)
	}
	if err := s.UpdateProfile(ctx, firstID, "Not/AZone", "09:00", "CaseUser"); err == nil {
		t.Fatal("invalid profile timezone was accepted")
	}
	if err := s.UpdateProfile(ctx, firstID, "  ", "09:00", "CaseUser"); err == nil {
		t.Fatal("empty profile timezone was accepted")
	}
	if err := s.UpdateProfile(ctx, firstID, "UTC", "not-a-time", "CaseUser"); err == nil {
		t.Fatal("invalid reminder time was accepted")
	}
	if err := s.UpdateProfile(ctx, firstID, "UTC", "09:00", "ab"); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("invalid profile username error=%v", err)
	}
	if err := s.UpdateProfile(ctx, firstID, "UTC", "09:00", "CaseUser"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProfile(ctx, firstID, "UTC", " 9:30 ", "CaseUser"); err != nil {
		t.Fatalf("legacy reminder input rejected: %v", err)
	}
	profile, err := s.UserByID(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ReminderTime != "09:30" {
		t.Fatalf("reminder time=%q, want canonical 09:30", profile.ReminderTime)
	}
	generatedID, err := s.CreateUser(ctx, "A+generated@example.com", "hash", "member", "UTC", "")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := s.UserByID(ctx, generatedID)
	if err != nil || generated.Username == "" {
		t.Fatalf("generated user=%#v err=%v", generated, err)
	}
}

func TestCreateUserFromInviteGeneratesAndValidatesUsernamesAtomically(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	adminID, err := s.CreateUser(ctx, "invite-admin@example.com", "hash", "admin", "UTC", "invite-admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "taken@example.com", "hash", "member", "UTC", "taken-name"); err != nil {
		t.Fatal(err)
	}

	// An omitted username is derived from the email local part and the
	// invitation remains transactional.
	generatedToken, err := s.CreateAuthToken(ctx, "invite", "Generated.User+1@example.com", nil, adminID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserFromInvite(ctx, generatedToken, "hash", "", "UTC"); err != nil {
		t.Fatal(err)
	}
	generated, err := s.UserByEmail(ctx, "generated.user+1@example.com")
	if err != nil || generated.Username == "" {
		t.Fatalf("generated user=%#v err=%v", generated, err)
	}

	duplicateToken, err := s.CreateAuthToken(ctx, "invite", "duplicate@example.com", nil, adminID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserFromInvite(ctx, duplicateToken, "hash", "TAKEN-NAME", "UTC"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate username error=%v", err)
	}
	// The failed uniqueness check must not consume the invitation.
	if err := s.CreateUserFromInvite(ctx, duplicateToken, "hash", "duplicate-name", "UTC"); err != nil {
		t.Fatalf("retry after duplicate error=%v", err)
	}

	expiredToken, err := s.CreateAuthToken(ctx, "invite", "expired@example.com", nil, adminID, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUserFromInvite(ctx, expiredToken, "hash", "expired-name", "UTC"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired invite error=%v", err)
	}
	if err := s.CreateUserFromInvite(ctx, duplicateToken, "hash", "another-name", "Not/AZone"); err == nil {
		t.Fatal("invalid timezone invite was accepted")
	}
}

func TestCredentialChangeRevokesTheCalendarFeedToken(t *testing.T) {
	// The calendar feed token is a year-long bearer credential in a plain URL,
	// usable without a session and without CSRF, exposing the member's upcoming
	// releases. A member changing their password after losing a device
	// reasonably expects that to end access, and the README tells them the
	// change revokes everything active.
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		change func(t *testing.T, s *Store, userID int64)
	}{
		{
			name: "password change",
			change: func(t *testing.T, s *Store, userID int64) {
				if err := s.UpdatePassword(ctx, userID, "new-hash"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "password reset",
			change: func(t *testing.T, s *Store, userID int64) {
				raw, err := s.CreateAuthToken(ctx, "reset", "feed-revoke@example.com", &userID, userID, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.ResetPasswordWithToken(ctx, raw, "new-hash"); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := testStore(t)
			userID, err := s.CreateUser(ctx, "feed-revoke@example.com", "hash", "member", "UTC", "feed-revoke")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := s.CreateCalendarFeedToken(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UserIDByCalendarFeedToken(ctx, raw); err != nil {
				t.Fatalf("the freshly minted token did not resolve: %v", err)
			}

			test.change(t, s, userID)

			if _, err := s.UserIDByCalendarFeedToken(ctx, raw); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("the calendar feed token still resolves after a credential change: err=%v", err)
			}
		})
	}
}

// TestSessionsCarryNoCsrfMaterial is #287. CreateSession minted a second random
// token per sign-in and Session selected it back on the hot path of every
// authenticated request, but nothing outside this package ever read it: the live
// CSRF implementation is an independent signed double-submit cookie that never
// consults the session.
func TestSessionsCarryNoCsrfMaterial(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "csrf-free@example.com", "hash", "member", "UTC", "csrf-free")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, userID, time.Hour); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='csrf_token'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatal("sessions still carries csrf_token, so every sign-in mints and stores material nothing reads")
	}
}
