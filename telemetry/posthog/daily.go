package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	"golang.org/x/sync/semaphore"
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
	sem     *semaphore.Weighted
	path    string
	pending *dailyReservation
	write   func(string, []byte, ...atomicfile.Option) error
}

type dailyReservation struct {
	key, day string
	accepted bool
}

const dailyClaimLimit = 35

// NewDailyClaims uses path and its adjacent lock file; its parent must exist.
func NewDailyClaims(path string) *DailyClaims {
	return &DailyClaims{path: path, sem: semaphore.NewWeighted(1)}
}

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

type dailyClaim struct {
	Day     string `json:"day"`
	Pending bool   `json:"pending,omitempty"`
}

type dailyState struct {
	Version int                     `json:"version"`
	Days    map[string][]dailyClaim `json:"days"`
}

type dailyLegacyState struct {
	Version int                 `json:"version"`
	Days    map[string][]string `json:"days"`
}

func (s *dailyState) compact() bool {
	changed := false
	for key, claims := range s.Days {
		accepted := 0
		for _, claim := range claims {
			if !claim.Pending {
				accepted++
			}
		}
		kept := slices.DeleteFunc(claims, func(claim dailyClaim) bool {
			if !claim.Pending && accepted > dailyClaimLimit {
				accepted--
				return true
			}
			return false
		})
		changed = changed || len(kept) != len(claims) || len(kept) == 0
		if len(kept) == 0 {
			delete(s.Days, key)
		} else {
			s.Days[key] = kept
		}
	}
	return changed
}

func newerDailyVersion(data []byte) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var number json.Number
	if token, err := decoder.Token(); err == nil && token == json.Delim('{') {
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				break
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				break
			}
			// Like encoding/json, a non-numeric duplicate leaves an earlier number in place.
			if !strings.EqualFold(key.(string), "version") {
				continue
			}
			if literal, ok := jsonNumber(value); ok {
				number = literal
			}
		}
	}
	if number != "" && number != "1" && number != "2" {
		version, parseErr := strconv.ParseFloat(string(number), 64)
		// Refuse unsupported numeric literals that may be at least 1; other shapes are damage.
		atLeastOne := parseErr == nil && version >= 1
		overflowed := errors.Is(parseErr, strconv.ErrRange) && math.IsInf(version, 1)
		return number, atLeastOne || overflowed
	}
	return "", false
}

func jsonNumber(value json.RawMessage) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", false
	}
	number, ok := decoded.(json.Number)
	return number, ok
}

func (d *DailyClaims) report(ctx context.Context, logger *slog.Logger, identity, event, key string, now func() time.Time, send func(time.Time) (Status, error)) (Status, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := d.sem.Acquire(ctx, 1); err != nil {
		return "", fmt.Errorf("lock daily telemetry: %w", err)
	}
	defer d.sem.Release(1)

	lock := flock.New(d.path + ".lock")
	_, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("lock daily telemetry: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	var state dailyState
	compacted := false
	data, err := os.ReadFile(d.path)
	if err == nil {
		if number, newer := newerDailyVersion(data); newer {
			return "", fmt.Errorf("unsupported daily telemetry version %s", number)
		}
		valid := json.Unmarshal(data, &state) == nil && state.Version == 2 && state.Days != nil
		if !valid {
			var legacy dailyLegacyState
			if json.Unmarshal(data, &legacy) == nil && legacy.Version == 1 && legacy.Days != nil {
				state = dailyState{Version: 2, Days: make(map[string][]dailyClaim)}
				for key, days := range legacy.Days {
					for _, day := range days {
						state.Days[key] = append(state.Days[key], dailyClaim{Day: day})
					}
				}
				valid, compacted = true, true
			}
		}
	validateDates:
		for _, days := range state.Days {
			for _, day := range days {
				if _, err := time.Parse(time.DateOnly, day.Day); err != nil {
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
		state = dailyState{Version: 2, Days: make(map[string][]dailyClaim)}
	}
	compacted = state.compact() || compacted

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
	settle := func() error {
		reservation := d.pending
		claims := state.Days[reservation.key]
		if reservation.accepted {
			for i, claim := range claims {
				if claim.Pending && claim.Day == reservation.day {
					claims[i].Pending = false
				}
			}
		} else {
			claims = slices.DeleteFunc(claims, func(claim dailyClaim) bool { return claim.Pending && claim.Day == reservation.day })
		}
		state.Days[reservation.key] = claims
		state.compact()
		err := save()
		if err == nil || errors.Is(err, atomicfile.ErrPublished) {
			d.pending = nil
			compacted = false
		}
		return err
	}
	if d.pending != nil {
		if err := settle(); err != nil {
			return "", fmt.Errorf("settle daily telemetry: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	encoded, _ := json.Marshal([]string{identity, event, key}) //nolint:errchkjson // strings always encode as JSON
	claim := string(encoded)
	timestamp := now().UTC()
	day := timestamp.Format(time.DateOnly)
	if slices.ContainsFunc(state.Days[claim], func(c dailyClaim) bool { return c.Day == day }) {
		if compacted {
			if err := save(); err != nil {
				logger.WarnContext(ctx, "compact daily telemetry failed", "error", err)
			}
		}
		return StatusSkipped, nil
	}
	pending := 0
	for _, c := range state.Days[claim] {
		if c.Pending {
			pending++
		}
	}
	if pending >= dailyClaimLimit {
		limitErr := fmt.Errorf("pending daily telemetry limit reached (%d); settle reservations or replace the claims file", dailyClaimLimit)
		if compacted {
			if err := save(); err != nil {
				return "", errors.Join(limitErr, fmt.Errorf("compact daily telemetry: %w", err))
			}
		}
		return "", limitErr
	}
	d.pending = &dailyReservation{key: claim, day: day}
	state.Days[claim] = append(state.Days[claim], dailyClaim{Day: day, Pending: true})
	if err := save(); err != nil {
		if errors.Is(err, atomicfile.ErrPublished) {
			releaseErr := settle()
			if d.pending != nil {
				return "", errors.Join(
					fmt.Errorf("reserve daily telemetry: %w", err),
					fmt.Errorf("release daily telemetry: %w", releaseErr),
				)
			}
		} else {
			d.pending = nil
		}
		return "", fmt.Errorf("reserve daily telemetry: %w", err)
	}
	status, captureErr := send(timestamp)
	if captureErr == nil && status == StatusQueued {
		d.pending.accepted = true
		if err := settle(); err != nil {
			logger.WarnContext(ctx, "accept daily telemetry failed", "error", err)
		}
		return status, nil
	}
	if err := settle(); err != nil {
		return "", errors.Join(captureErr, fmt.Errorf("release daily telemetry: %w", err))
	}
	return status, captureErr
}
