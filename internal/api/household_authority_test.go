package api

import (
	"github.com/pjunod/monarr/internal/domain"
	"testing"
)

// The account may remain an owner when a deliberately restricted integration
// session is used. Never derive that session's authority from the account alone.
func TestHouseholdSessionAuthority(t *testing.T) {
	for _, tc := range []struct{ method, accountRole, want string }{
		{"local", "owner", "owner"}, {"local", "member", "member"},
		{"cinema", "owner", "member"}, {"cinema", "member", "member"},
		{"machine", "owner", "owner"}, {"open", "owner", "owner"},
		{"unknown", "owner", "member"}, {"", "owner", "member"},
	} {
		t.Run(tc.method+"/"+tc.accountRole, func(t *testing.T) {
			p := principal{Method: tc.method, Account: domain.Account{Role: tc.accountRole}}
			if got := p.accessRole(); got != tc.want {
				t.Fatalf("session authority = %q; want %q", got, tc.want)
			}
			if p.Account.Role != tc.accountRole {
				t.Fatal("session authority changed permanent account role")
			}
		})
	}
}
