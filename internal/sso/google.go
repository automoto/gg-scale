package sso

import (
	"encoding/json"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

const googleProfileURL = "https://openidconnect.googleapis.com/v1/userinfo"

// Google returns the Google provider. redirectURL is the fixed callback URL
// registered on the Google OAuth app.
func Google(clientID, clientSecret, redirectURL string) Provider {
	return Provider{
		Name:  "google",
		Label: "Google",
		OAuth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     endpoints.Google,
			Scopes:       []string{"openid", "email", "profile"},
		},
		ProfileURL: googleProfileURL,
		Parse:      parseGoogle,
	}
}

// parseGoogle drops an unverified email: only a verified one can identify
// or seed an account.
func parseGoogle(body []byte) (Identity, error) {
	var profile struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return Identity{}, err
	}
	id := Identity{Subject: profile.Sub, Name: profile.Name}
	if profile.EmailVerified && profile.Email != "" {
		id.Email = profile.Email
		id.EmailVerified = true
	}
	return id, nil
}
