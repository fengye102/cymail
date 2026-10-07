package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminSetupLoginAndLogout(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "admin-auth.json")
	srv := newTestServer(t, Config{AdminAuthFile: authFile})

	status := performAdminRequest(t, srv, http.MethodGet, "/api/auth/status", "", nil)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"initialized":false`) {
		t.Fatalf("initial status=%d body=%s", status.Code, status.Body.String())
	}

	setupBody := `{"username":"local-admin","password":"Test-password-2026!"}`
	setup := performAdminRequest(t, srv, http.MethodPost, "/api/auth/setup", setupBody, nil)
	if setup.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", setup.Code, setup.Body.String())
	}
	cookies := setup.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminSessionCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %#v", cookies)
	}
	raw, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Test-password-2026!") {
		t.Fatal("credential file contains the plaintext password")
	}
	var stored adminCredentialFile
	if err := json.Unmarshal(raw, &stored); err != nil || stored.PasswordHash == "" || stored.Salt == "" {
		t.Fatalf("invalid stored credential: err=%v", err)
	}

	protected := performAdminRequest(t, srv, http.MethodGet, "/api/accounts", "", cookies[0])
	if protected.Code != http.StatusOK {
		t.Fatalf("session protected request status=%d body=%s", protected.Code, protected.Body.String())
	}

	logout := performAdminRequest(t, srv, http.MethodPost, "/api/auth/logout", `{}`, cookies[0])
	if logout.Code != http.StatusOK {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}
	afterLogout := performAdminRequest(t, srv, http.MethodGet, "/api/accounts", "", cookies[0])
	if afterLogout.Code != http.StatusUnauthorized {
		t.Fatalf("request after logout status=%d, want 401", afterLogout.Code)
	}

	restarted := newTestServer(t, Config{AdminAuthFile: authFile})
	login := performAdminRequest(t, restarted, http.MethodPost, "/api/auth/login", setupBody, nil)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login after restart status=%d body=%s", login.Code, login.Body.String())
	}
	wrong := performAdminRequest(t, restarted, http.MethodPost, "/api/auth/login", `{"username":"local-admin","password":"wrong-password"}`, nil)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status=%d, want 401", wrong.Code)
	}
}

func TestAdminSetupRejectsForwardedRemoteClient(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "admin-auth.json")
	srv := newTestServer(t, Config{AdminAuthFile: authFile})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{"username":"admin","password":"Test-password-2026!"}`))
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("forwarded remote setup status=%d, want 403", resp.Code)
	}
}

func performAdminRequest(t *testing.T, srv *Server, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, req)
	return resp
}
