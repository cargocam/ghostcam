package main

import (
	"net/http"
	"testing"
)

// TestAdminAuthDecision exercises the pure decision matrix for the
// adminAuth middleware. The middleware itself is a thin dispatch over
// this function plus cookie parsing + DB lookup.
func TestAdminAuthDecision(t *testing.T) {
	tests := []struct {
		name     string
		jwtValid bool
		isAdmin  bool
		dbErr    error
		want     int
	}{
		{
			name:     "no cookie or invalid JWT → 401",
			jwtValid: false,
			want:     http.StatusUnauthorized,
		},
		{
			name:     "invalid JWT takes precedence over admins-table state",
			jwtValid: false,
			isAdmin:  true,
			want:     http.StatusUnauthorized,
		},
		{
			name:     "valid JWT but DB error → 500",
			jwtValid: true,
			dbErr:    errAdminCheck,
			want:     http.StatusInternalServerError,
		},
		{
			name:     "valid JWT but user not in admins → 403",
			jwtValid: true,
			isAdmin:  false,
			want:     http.StatusForbidden,
		},
		{
			name:     "valid JWT and user is admin → 200 (run handler)",
			jwtValid: true,
			isAdmin:  true,
			want:     http.StatusOK,
		},
		{
			name:     "DB error beats !isAdmin (operator sees 500, not 403)",
			jwtValid: true,
			isAdmin:  false,
			dbErr:    errAdminCheck,
			want:     http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adminAuthDecision(tt.jwtValid, tt.isAdmin, tt.dbErr)
			if got != tt.want {
				t.Errorf("adminAuthDecision(%v, %v, %v) = %d, want %d",
					tt.jwtValid, tt.isAdmin, tt.dbErr, got, tt.want)
			}
		})
	}
}
