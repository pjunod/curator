package domain

import "time"

// Account is Curator's local identity. External subjects are explicit links,
// never the owner of household history or a substitute for local permissions.
type Account struct {
	Revision             int64     `json:"revision"`
	ID                   int64     `json:"id"`
	Username             string    `json:"username"`
	DisplayName          string    `json:"displayName"`
	Role                 string    `json:"role"`
	Status               string    `json:"status"`
	CreatedAt            time.Time `json:"createdAt"`
	PasswordHash         string    `json:"-"`
	LegacySalt           string    `json:"-"`
	CredentialGeneration int64     `json:"-"`
}
type AccountSession struct {
	Account                Account
	Method                 string
	IntegrationGrant       string
	LinkID, LinkGeneration int64
	CreatedAt, ExpiresAt   time.Time
}
