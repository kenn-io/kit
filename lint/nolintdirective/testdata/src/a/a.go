package a

//nolint:kennlint // sleeptest: waits on a real process
var sleep int

//nolint:kennlint // deadlinetest: expiry is the expected result
var deadline int

//nolint:errcheck,kennlint // errtext: external stderr has no typed error
var multiple int

//nolint:errcheck // unrelated
var unrelated int

// This prose mentions //nolint:kennlint without declaring a directive.
var prose int

//nolint:kennlint // waits on a real process // want "must name a kit rule and a reason"
var missingRule int

//nolint:kennlint // unknown: a reason // want "must name a kit rule and a reason"
var unknown int

// want +2 "must name a kit rule and a reason"

//nolint:kennlint // sleeptest:
var missingReason int

// want +2 "must name a kit rule and a reason"

//nolint:kennlint
var missingExplanation int

// nolint:kennlint // a broad reason // want "must name a kit rule and a reason"
var spaced int

//nolint:kennlint // consent expiry uses the real clock // want "must name a kit rule and a reason"
var expiry int

//nolint:kennlint // outlasts the real socket read deadline // want "must name a kit rule and a reason"
var socket int

// nolint:KENNLINT // missing rule // want "must name a kit rule and a reason"
var uppercase int
