package store

import (
	"context"
	"testing"
	"time"

	"github.com/djbu/corral/internal/apitoken"
)

func TestMigration0005_AppliesCleanly(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	v, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 10 {
		t.Fatalf("SchemaVersion = %d, want 10 (0005_api_tokens.sql and later applied)", v)
	}

	var n int
	if err := st.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='api_tokens'",
	).Scan(&n); err != nil {
		t.Fatalf("checking table api_tokens exists: %v", err)
	}
	if n != 1 {
		t.Fatalf("table api_tokens does not exist after migration")
	}
}

func TestStore_CreateVerifyToken_RoundTrips(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	pt, hash, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := st.CreateToken(ctx, "tok1", hash, "laptop", "admin", nil); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	row, ok, err := st.VerifyToken(ctx, pt)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !ok {
		t.Fatalf("VerifyToken ok = false, want true")
	}
	if row.ID != "tok1" {
		t.Fatalf("row.ID = %q, want tok1", row.ID)
	}
	if row.Label != "laptop" {
		t.Fatalf("row.Label = %q, want laptop", row.Label)
	}
	if row.Scope != "admin" {
		t.Fatalf("row.Scope = %q, want admin", row.Scope)
	}
	if row.SessionID != "" {
		t.Fatalf("row.SessionID = %q, want empty for admin token", row.SessionID)
	}
	if row.RevokedMs != 0 {
		t.Fatalf("row.RevokedMs = %d, want 0 (active)", row.RevokedMs)
	}
}

func TestStore_VerifyToken_NeverCreated(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	pt, _, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}

	row, ok, err := st.VerifyToken(ctx, pt)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if ok {
		t.Fatalf("VerifyToken ok = true, want false for a plaintext never created")
	}
	if row != (TokenRow{}) {
		t.Fatalf("VerifyToken row = %+v, want zero value on miss", row)
	}
}

func TestStore_RevokeToken_FailsSubsequentVerify(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	pt, hash, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := st.CreateToken(ctx, "tok1", hash, "laptop", "admin", nil); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if err := st.RevokeToken(ctx, "tok1"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	_, ok, err := st.VerifyToken(ctx, pt)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if ok {
		t.Fatalf("VerifyToken ok = true after revoke, want false")
	}
}

func TestStore_RevokeToken_Idempotent(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	_, hash, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := st.CreateToken(ctx, "tok1", hash, "laptop", "admin", nil); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if err := st.RevokeToken(ctx, "tok1"); err != nil {
		t.Fatalf("first RevokeToken: %v", err)
	}
	if err := st.RevokeToken(ctx, "tok1"); err != nil {
		t.Fatalf("second RevokeToken (idempotent call): %v", err)
	}
}

func TestStore_ListTokens_ReturnsRowsWithNoHashField(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	_, hash1, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := st.CreateToken(ctx, "tok1", hash1, "laptop", "admin", nil); err != nil {
		t.Fatalf("CreateToken tok1: %v", err)
	}
	_, hash2, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := st.CreateToken(ctx, "tok2", hash2, "phone", "admin", nil); err != nil {
		t.Fatalf("CreateToken tok2: %v", err)
	}

	// Exercise the revoked_ms and last_used_ms scan paths too — VerifyToken
	// never surfaces a revoked row's RevokedMs (it returns early on a
	// revoked match), so ListTokens is the only place that assignment is
	// ever reached. A revoked token must still appear in the list (`token
	// list` shows revoked tokens too), just with RevokedMs set.
	fc.Advance(time.Minute)
	st.TouchToken(ctx, "tok2")
	if err := st.RevokeToken(ctx, "tok1"); err != nil {
		t.Fatalf("RevokeToken tok1: %v", err)
	}

	rows, err := st.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListTokens returned %d rows, want 2 (revoked tokens still list)", len(rows))
	}
	if rows[0].ID != "tok1" || rows[1].ID != "tok2" {
		t.Fatalf("ListTokens order = [%s, %s], want [tok1, tok2] (created_ms ASC)", rows[0].ID, rows[1].ID)
	}
	if rows[0].RevokedMs == 0 {
		t.Fatalf("rows[0] (tok1, revoked).RevokedMs = 0, want non-zero")
	}
	if rows[1].RevokedMs != 0 {
		t.Fatalf("rows[1] (tok2, active).RevokedMs = %d, want 0", rows[1].RevokedMs)
	}
	if rows[1].LastUsedMs != fc.Now().UnixMilli() {
		t.Fatalf("rows[1] (tok2).LastUsedMs = %d, want %d", rows[1].LastUsedMs, fc.Now().UnixMilli())
	}
	if rows[0].LastUsedMs != 0 {
		t.Fatalf("rows[0] (tok1, never touched).LastUsedMs = %d, want 0", rows[0].LastUsedMs)
	}
	// TokenRow has no field for token_hash by construction (see its doc
	// comment) — there is nothing to assert at the Go level beyond the
	// type not compiling with one; this test documents that intent.
}

func TestStore_CreateToken_SessionScopedRoundTripsSessionID(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	pt, hash, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	sessionID := "sess-42"
	if err := st.CreateToken(ctx, "tok1", hash, "scoped", "session", &sessionID); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	row, ok, err := st.VerifyToken(ctx, pt)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !ok {
		t.Fatalf("VerifyToken ok = false, want true")
	}
	if row.Scope != "session" {
		t.Fatalf("row.Scope = %q, want session", row.Scope)
	}
	if row.SessionID != "sess-42" {
		t.Fatalf("row.SessionID = %q, want sess-42", row.SessionID)
	}
}

func TestStore_TouchToken_UpdatesLastUsedMs(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()

	pt, hash, err := apitoken.Mint()
	if err != nil {
		t.Fatalf("apitoken.Mint: %v", err)
	}
	if err := st.CreateToken(ctx, "tok1", hash, "laptop", "admin", nil); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	fc.Advance(time.Minute)
	st.TouchToken(ctx, "tok1")

	row, ok, err := st.VerifyToken(ctx, pt)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if !ok {
		t.Fatalf("VerifyToken ok = false, want true")
	}
	if row.LastUsedMs != fc.Now().UnixMilli() {
		t.Fatalf("row.LastUsedMs = %d, want %d", row.LastUsedMs, fc.Now().UnixMilli())
	}
}
