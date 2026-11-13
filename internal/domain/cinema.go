package domain

// CinemaConnection is backend-only. Account and grant secrets never serialize.
type CinemaConnection struct {
	BaseURL, Issuer, ClientID, Redirect string
	ClientSecret                        string `json:"-"`
	Generation                          int64
}
type CinemaLink struct {
	ID, AccountID, ConnectionGeneration, Generation int64
	Issuer, Subject                                 string
	Grant                                           string `json:"-"` // transient per-session grant, never part of a stored link
}
type CinemaHandoff struct {
	StateDigest, Mode, SessionDigest, Verifier, CookieDigest, NativeChallenge, Claim, Grant string `json:"-"`
	AccountID, CredentialGeneration, ConnectionGeneration, ReadyLink, ExpiresAt             int64
}
type CinemaRevocation struct {
	ID                  int64
	BaseURL, ClientID   string
	ClientSecret, Grant string `json:"-"`
}
