package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

const (
	testUsername = "testuser"
	testPassword = "testpass"
)

// authedTransport injects basic-auth credentials into every request so tests
// can keep using the plain http.Get/http.Post helpers against the API.
type authedTransport struct {
	http.RoundTripper
}

func (t authedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.SetBasicAuth(testUsername, testPassword)
	return t.RoundTripper.RoundTrip(req2)
}

// authedClient returns an http.Client that sends the shared test credentials
// on every request.
func authedClient() *http.Client {
	return &http.Client{Transport: authedTransport{http.DefaultTransport}}
}

// authedRequest builds a request carrying the shared test credentials, for use
// with httptest.NewRecorder against a mux directly.
func authedRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.SetBasicAuth(testUsername, testPassword)
	return req
}

// resetAuthTables drops and recreates the schema so each auth test starts from
// a clean users table (and empty data tables).
func resetAuthTables(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS users; DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))
}

// TestAuthRequired ensures requests without a basic-auth header are rejected.
func TestAuthRequired(t *testing.T) {
	resetAuthTables(t)
	mux := routes()

	req := httptest.NewRequest(http.MethodGet, "/api/splits", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Basic realm="auth"`, rec.Header().Get("WWW-Authenticate"))
}

// TestAuthCreatesUnknownUser ensures a brand-new user is created and logged in.
func TestAuthCreatesUnknownUser(t *testing.T) {
	resetAuthTables(t)
	mux := routes()

	req := authedRequest(t, http.MethodGet, "/api/splits", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	user, err := fetchUserByName(testUsername)
	require.NoError(t, err)
	require.Equal(t, testUsername, user.Name)
	require.True(t, userPasswordMatches(user, testPassword))
}

// TestAuthWrongPassword ensures a known user with a wrong password is rejected.
func TestAuthWrongPassword(t *testing.T) {
	resetAuthTables(t)
	require.NoError(t, createUser(testUsername, testPassword))
	mux := routes()

	req := httptest.NewRequest(http.MethodGet, "/api/splits", nil)
	req.SetBasicAuth(testUsername, "wrongpass")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Basic realm="auth"`, rec.Header().Get("WWW-Authenticate"))
}

// TestAuthKnownUserCorrectPassword ensures a known user with the right password
// is allowed through.
func TestAuthKnownUserCorrectPassword(t *testing.T) {
	resetAuthTables(t)
	require.NoError(t, createUser(testUsername, testPassword))
	mux := routes()

	req := authedRequest(t, http.MethodGet, "/api/splits", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAuthCreatedUserLoginNeverExpires ensures a created user can authenticate
// again later (the account and its login persist).
func TestAuthCreatedUserLoginNeverExpires(t *testing.T) {
	resetAuthTables(t)

	// First request creates the user.
	mux := routes()
	req := authedRequest(t, http.MethodGet, "/api/splits", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// A later request with the same credentials is still allowed.
	req = authedRequest(t, http.MethodGet, "/api/splits", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
