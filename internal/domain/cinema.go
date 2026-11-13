package domain

type CinemaConnection struct {
	BaseURL, Issuer, ClientID, ClientSecret, Redirect string
	Generation                                        int64
}
type CinemaLink struct {
	ID, AccountID, ConnectionGeneration, Generation int64
	Issuer, Subject, Grant                          string
}
type CinemaHandoff struct {
	StateDigest, Mode, SessionDigest, Verifier, CookieDigest, NativeChallenge, Claim, Grant string
	AccountID, CredentialGeneration, ConnectionGeneration, ReadyLink, ExpiresAt             int64
}
type CinemaRevocation struct {
	ID                                     int64
	BaseURL, ClientID, ClientSecret, Grant string
}
