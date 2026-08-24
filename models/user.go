package models

// User is one row in the users table. Password holds the bcrypt hash of the
// user's basic-auth password; it is never stored in plaintext.
type User struct {
	ID       int64
	Name     string
	Password string
}