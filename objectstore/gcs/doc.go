// Package gcs stores immutable bytes through Google Cloud Storage's XML API.
//
// Config requires explicit GCS HMAC key ID and secret; these are not OAuth
// tokens, service-account JSON credentials, or ambient AWS credentials. Requests
// use the existing AWS V4 signer with Google's documented interoperability
// scope. Construction makes no requests and requires no mutating probes.
//
// Publish creates a resumable upload with a zero-generation precondition. A
// normal receipt verifies stored generation, length, and full-object CRC32C.
// Existing keys and ambiguous completion require generation-guarded full SHA-256
// readback. Failed calls return a zero receipt. ErrOutcomeUnknown requires the
// caller to retain its intent and reconcile; it does not authorize deletion.
// Source readers remain caller-owned and must stay stable until Publish returns.
// All recovery and session cleanup share the caller's context.
//
// Revision is the object's opaque generation. OpenRange and Delete honor a
// supplied generation precondition. Inventory includes foreign names and grants
// no cleanup authority. Tests use explicit loopback fixtures; actual provider
// behavior needs separately authorized disposable-namespace contract testing.
//
// Live contract entry point (requires separate namespace authorization):
//
//	go test -tags=gcslive -shuffle=on ./objectstore/gcs -run '^TestGCSLiveContract$'
//
// It requires KIT_GCS_TEST_LIVE=1 and explicit KIT_GCS_TEST_BUCKET,
// KIT_GCS_TEST_PREFIX, KIT_GCS_TEST_KEY_ID, KIT_GCS_TEST_SECRET, and
// KIT_GCS_TEST_ENDPOINT settings. Use a disposable bucket/prefix and GCS HMAC
// credentials with object create/get/list/delete permissions. The test appends
// a random namespace and removes only keys with verified publication receipts.
package gcs
