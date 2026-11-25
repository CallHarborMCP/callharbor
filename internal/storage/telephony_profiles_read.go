package storage

// TelephonyProfile is the owner-scoped read model for a telephony profile.
// Secret references are deliberately omitted: no read model exposes them.
// UsableTelephonyProfile embeds it.
type TelephonyProfile struct {
	ID                 string
	OwnerUserID        string
	Name               string
	Provider           string
	CallerNumber       string
	ConnectionID       string
	ConnectivityStatus string
	ConnectivityReason string
	CreatedAt          string
	UpdatedAt          string
}
