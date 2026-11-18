package posthog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// Consent is the user's recorded telemetry choice.
type Consent uint8

const (
	ConsentUnanswered Consent = iota
	ConsentGranted
	ConsentDenied
)

var startupConsent = func() Consent {
	consent, _ := ReadConsent()
	return consent
}()

func consentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".kenn", "telemetry-consent"), nil
}

// ReadConsent reads the current shared choice without creating files.
// Missing and malformed choices are unanswered.
func ReadConsent() (Consent, error) {
	path, err := consentPath()
	if err != nil {
		return ConsentUnanswered, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ConsentUnanswered, nil
	}
	if err != nil {
		return ConsentUnanswered, fmt.Errorf("read telemetry consent: %w", err)
	}
	switch string(data) {
	case "yes\n":
		return ConsentGranted, nil
	case "no\n":
		return ConsentDenied, nil
	default:
		return ConsentUnanswered, nil
	}
}

// RecordConsent records the shared choice. Only the application that owns the
// consent flow should write it; reporting apps read the choice and do not ask.
// Errors may wrap atomicfile.ErrPublished or atomicfile.ErrNotDurable when
// publication or durability is uncertain.
func RecordConsent(granted bool) error {
	path, err := consentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prepare telemetry consent directory: %w", err)
	}
	data := "no\n"
	if granted {
		data = "yes\n"
	}
	if err := atomicfile.WriteFile(path, []byte(data), atomicfile.WithPrivate()); err != nil {
		return fmt.Errorf("record telemetry consent: %w", err)
	}
	return nil
}
