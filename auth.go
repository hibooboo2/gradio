package main

import (
	"log/slog"
	"net/http"

	"github.com/hibooboo2/gradio/db"
	"github.com/jackc/pgx/v5"
)

// requireAuth enforces HTTP basic auth on the UI and API. Every request must
// carry a basic-auth Authorization header:
//
//   - If the header is missing/empty the request is rejected with 401.
//   - The basic-auth username is used to look up the user.
//   - If a user with that name exists, the supplied password must match its
//     stored password or the request is rejected with 401.
//   - If no user with that name exists, one is created with the supplied
//     password and the request is allowed (the user is logged in).
//
// Logins never expire: basic auth credentials are carried on every request and
// created users persist in the database.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="auth"`)
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		user, err := db.FetchUserByName(r.Context(), username)
		switch {
		case err == pgx.ErrNoRows:
			// Unknown user: create one and log them in.
			if err := db.CreateUser(r.Context(), username, password); err != nil {
				slog.ErrorContext(r.Context(), "create user", "err", err, "user", username)
				writeError(w, http.StatusInternalServerError, "failed to create user")
				return
			}
			slog.InfoContext(r.Context(), "created user", "user", username)
			next.ServeHTTP(w, r)
			return
		case err != nil:
			slog.ErrorContext(r.Context(), "lookup user", "err", err, "user", username)
			writeError(w, http.StatusInternalServerError, "failed to look up user")
			return
		}

		// Known user: the password must match.
		if !db.UserPasswordMatches(user, password) {
			w.Header().Set("WWW-Authenticate", `Basic realm="auth"`)
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}

		next.ServeHTTP(w, r)
	})
}
