package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
)

// Status describes SDK queue acceptance, daily deduplication, or disabled telemetry.
type Status string

const (
	StatusQueued   Status = "queued"
	StatusSkipped  Status = "skipped"
	StatusDisabled Status = "disabled"
)

// DailyClaims persists installation-scoped UTC daily claims at a caller-owned path.
type DailyClaims struct {
	path    string
	pending *dailyReservation
	write   func(string, []byte, ...atomicfile.Option) error
}

type dailyReservation struct{ key, day string }

// NewDailyClaims uses path and its adjacent lock file; its parent must exist.
func NewDailyClaims(path string) *DailyClaims { return &DailyClaims{path: path} }

type dailyEvent struct {
	key    string
	claims *DailyClaims
}

// WithDailyEvent counts each filtered key once per installation and UTC day.
func WithDailyEvent(event, keyProperty string, claims *DailyClaims) Option {
	return postHogOptionFunc(func(c *postHogReporterConfig) {
		if c.dailyEvents == nil {
			c.dailyEvents = make(map[string]dailyEvent)
		}
		c.dailyEvents[strings.TrimSpace(event)] = dailyEvent{strings.TrimSpace(keyProperty), claims}
	})
}

type dailyState struct {
	Version int                 `json:"version"`
	Days    map[string][]string `json:"days"`
}

func newerDailyVersion(data []byte) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if token, err := decoder.Token(); err == nil && token == json.Delim('{') {
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				break
			}
			if key == "version" {
				token, err := decoder.Token()
				if number, ok := token.(json.Number); err == nil && ok && number != "1" {
					version, parseErr := strconv.ParseFloat(string(number), 64)
					// Refuse integer versions above 1 and other numeric literals we can't prove at most 1; strings and other shapes are damage.
					return number, (parseErr == nil && version >= 1) || (errors.Is(parseErr, strconv.ErrRange) && math.IsInf(version, 1))
				}
				break
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				break
			}
		}
	}
	return "", false
}

func (d *DailyClaims) report(ctx context.Context, identity, event, key string, now func() time.Time, send func(time.Time) (Status, error)) (Status, error) {
	lock := flock.New(d.path + ".lock")
	_, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("lock daily telemetry: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	var state dailyState
	data, err := os.ReadFile(d.path)
	if err == nil {
		if number, newer := newerDailyVersion(data); newer {
			return "", fmt.Errorf("unsupported daily telemetry version %s", number)
		}
		valid := json.Unmarshal(data, &state) == nil && state.Version == 1 && state.Days != nil
	validateDates:
		for _, days := range state.Days {
			for _, day := range days {
				if _, err := time.Parse(time.DateOnly, day); err != nil {
					valid = false
					break validateDates
				}
			}
		}
		if !valid {
			state = dailyState{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read daily telemetry: %w", err)
	}
	if state.Days == nil {
		state = dailyState{Version: 1, Days: make(map[string][]string)}
	}
	save := func() error {
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		write := d.write
		if write == nil {
			write = atomicfile.WriteFile
		}
		return write(d.path, data, atomicfile.WithPrivate())
	}
	release := func(reservation dailyReservation) {
		state.Days[reservation.key] = slices.DeleteFunc(state.Days[reservation.key], func(day string) bool { return day == reservation.day })
		if len(state.Days[reservation.key]) == 0 {
			delete(state.Days, reservation.key)
		}
	}
	rollback := func() error {
		release(*d.pending)
		err := save()
		if err == nil || errors.Is(err, atomicfile.ErrPublished) {
			d.pending = nil
		}
		return err
	}
	if d.pending != nil {
		if err := rollback(); err != nil {
			return "", fmt.Errorf("release daily telemetry: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	encoded, _ := json.Marshal([]string{identity, event, key}) //nolint:errchkjson // strings always encode as JSON
	claim := string(encoded)
	timestamp := now().UTC()
	day := timestamp.Format(time.DateOnly)
	if slices.Contains(state.Days[claim], day) {
		return StatusSkipped, nil
	}
	d.pending = &dailyReservation{key: claim, day: day}
	state.Days[claim] = append(state.Days[claim], day)
	if err := save(); err != nil {
		if errors.Is(err, atomicfile.ErrPublished) {
			if releaseErr := rollback(); d.pending != nil {
				return "", errors.Join(fmt.Errorf("reserve daily telemetry: %w", err), fmt.Errorf("release daily telemetry: %w", releaseErr))
			}
		} else {
			d.pending = nil
		}
		return "", fmt.Errorf("reserve daily telemetry: %w", err)
	}
	status, captureErr := send(timestamp)
	if captureErr == nil && status == StatusQueued {
		d.pending = nil
		return status, nil
	}
	if err := rollback(); err != nil {
		return "", errors.Join(captureErr, fmt.Errorf("release daily telemetry: %w", err))
	}
	return status, captureErr
}
