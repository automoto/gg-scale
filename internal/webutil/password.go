package webutil

import "golang.org/x/crypto/bcrypt"

// PasswordMatches reports whether password matches hash. An account that
// signs in only through a provider has no hash and never matches. bcrypt
// returns at once on an empty hash, so the compare then runs against dummy:
// the response time must not show that an account has no password.
func PasswordMatches(hash, dummy []byte, password string) bool {
	if len(hash) == 0 {
		_ = bcrypt.CompareHashAndPassword(dummy, []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}
