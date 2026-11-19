package embedlocal

import "time"

// BatchItems and RequestTimeout are transport defaults for the built-in
// server. CPU inference processes one input at a time across all apps.
// Apps should use these when the endpoint is the built-in server discovered
// by this package and the user has not set their own values. Send one request
// at a time to this server.
const (
	BatchItems     = 8
	RequestTimeout = 5 * time.Minute
)
