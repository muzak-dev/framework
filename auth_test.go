package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecureCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		given    string
		expected string
		want     bool
	}{
		{"equal", "secret", "secret", true},
		{"different", "secret", "guessed", false},
		{"prefix of the expected", "sec", "secret", false},
		{"expected is a prefix", "secret", "sec", false},
		{"both empty", "", "", true},
		{"empty given", "", "secret", false},
		{"case matters", "Secret", "secret", false},
		{"long values", strings.Repeat("a", 1000), strings.Repeat("a", 1000), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SecureCompare(tc.given, tc.expected); got != tc.want {
				t.Errorf("SecureCompare(%q, %q) = %v, want %v", tc.given, tc.expected, got, tc.want)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"standard", "Bearer abc123", "abc123", true},
		{"lowercase scheme", "bearer abc123", "abc123", true},
		{"mixed case scheme", "BeArEr abc123", "abc123", true},
		{"padded token", "Bearer   abc123  ", "abc123", true},
		{"absent", "", "", false},
		{"wrong scheme", "Basic abc123", "", false},
		{"no space", "Bearerabc123", "", false},
		{"empty token", "Bearer ", "", false},
		{"only whitespace", "Bearer    ", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", "/x", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			ctx := &Context{r: req}
			got, ok := BearerToken(ctx)
			if got != tc.want || ok != tc.ok {
				t.Errorf("BearerToken(%q) = %q, %v; want %q, %v", tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRequireBearerToken(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(RequireBearerToken("the-real-token")))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	tests := []struct {
		name       string
		header     string
		status     int
		challenged bool
	}{
		{"correct token", "Bearer the-real-token", http.StatusOK, false},
		{"wrong token", "Bearer a-guess", http.StatusUnauthorized, false},
		{"missing credential", "", http.StatusUnauthorized, true},
		{"wrong scheme", "Basic the-real-token", http.StatusUnauthorized, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", "/x", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := doRequest(t, app, req)
			assertStatus(t, rec, tc.status)

			challenge := rec.Header().Get("WWW-Authenticate")
			if tc.challenged && challenge == "" {
				t.Error("no WWW-Authenticate challenge was issued")
			}
			if !tc.challenged && challenge != "" {
				t.Errorf("an unexpected challenge was issued: %q", challenge)
			}
			if tc.status != http.StatusOK {
				body := rec.Body.String()
				if strings.Contains(body, "the-real-token") {
					t.Errorf("the response leaked the expected token: %s", body)
				}
			}
		})
	}
}

func TestRequireHeaderToken(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(RequireHeaderToken("X-Token", "coneofsilence")))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	tests := []struct {
		name   string
		value  string
		status int
		want   string
	}{
		{"correct", "coneofsilence", http.StatusOK, ""},
		{"wrong", "hailhydra", http.StatusUnauthorized, "the X-Token header is not valid"},
		{"missing", "", http.StatusUnauthorized, "the X-Token header is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", "/x", nil)
			if tc.value != "" {
				req.Header.Set("X-Token", tc.value)
			}
			rec := doRequest(t, app, req)
			assertStatus(t, rec, tc.status)
			if tc.want != "" {
				if got := decodeError(t, rec).Error.Message; got != tc.want {
					t.Errorf("message = %q, want %q", got, tc.want)
				}
			}
			if strings.Contains(rec.Body.String(), "coneofsilence") {
				t.Errorf("the response leaked the expected token: %s", rec.Body.String())
			}
		})
	}
}
