package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerToken(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"Basic abc":   "",
		"":            "",
		"Bearer  abc": "abc",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", in)
		if got := bearerToken(r); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestRequireRole(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := requireRole("editor", ok)

	for _, tc := range []struct {
		roles []string
		want  int
	}{
		{[]string{"editor", "viewer"}, http.StatusNoContent},
		{[]string{"viewer"}, http.StatusForbidden},
		{nil, http.StatusForbidden},
	} {
		r := httptest.NewRequest("POST", "/api/notes", nil)
		r = r.WithContext(context.WithValue(r.Context(), claimsKey{}, &claims{Subject: "u", Roles: tc.roles}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("roles=%v → %d, want %d", tc.roles, rec.Code, tc.want)
		}
	}
}
