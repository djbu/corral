package store

import (
	"context"
	"testing"
	"time"
)

func TestClaimIncomingMessage_IdempotentAndExpires(t *testing.T) {
	st, fc := openTestStore(t)
	ctx := context.Background()
	now := fc.Now().UnixMilli()

	claimed, err := st.ClaimIncomingMessage(ctx, "telegram", "hash-a", now, now+1000)
	if err != nil || !claimed {
		t.Fatalf("first ClaimIncomingMessage = %v, %v; want true, nil", claimed, err)
	}
	claimed, err = st.ClaimIncomingMessage(ctx, "telegram", "hash-a", now, now+1000)
	if err != nil || claimed {
		t.Fatalf("duplicate ClaimIncomingMessage = %v, %v; want false, nil", claimed, err)
	}
	// A different backend owns an independent provider identity namespace.
	claimed, err = st.ClaimIncomingMessage(ctx, "slack", "hash-a", now, now+1000)
	if err != nil || !claimed {
		t.Fatalf("other-backend ClaimIncomingMessage = %v, %v; want true, nil", claimed, err)
	}

	fc.Advance(time.Second)
	later := fc.Now().UnixMilli()
	claimed, err = st.ClaimIncomingMessage(ctx, "telegram", "hash-a", later, later+1000)
	if err != nil || !claimed {
		t.Fatalf("expired ClaimIncomingMessage = %v, %v; want true, nil", claimed, err)
	}
}

func TestClaimIncomingMessage_RejectsInvalidArguments(t *testing.T) {
	st, fc := openTestStore(t)
	now := fc.Now().UnixMilli()
	for _, tc := range []struct {
		backend, hash string
		expiry        int64
	}{
		{"", "hash", now + 1},
		{"slack", "", now + 1},
		{"slack", "hash", now},
	} {
		if _, err := st.ClaimIncomingMessage(context.Background(), tc.backend, tc.hash, now, tc.expiry); err == nil {
			t.Fatalf("ClaimIncomingMessage(%q, %q, expiry=%d) succeeded", tc.backend, tc.hash, tc.expiry)
		}
	}
}
