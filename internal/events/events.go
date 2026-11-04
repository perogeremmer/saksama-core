// Package events implements the event contract between the core and every
// frontend.
//
// The contract is the product API: the TUI, the CLI, the future web UI, and any
// third-party app builder all consume the same JSONL stream. That only stays
// true if the envelope, sequence numbering, and redaction are enforced in one
// place, which is here.
package events

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ContractVersion is the version of the envelope. It only changes in a
// compatible way, or by introducing new event types.
const ContractVersion = 1

// Event is one line of the JSONL stream.
type Event struct {
	ContractVersion int            `json:"contract_version"`
	Seq             int64          `json:"seq"`
	TS              float64        `json:"ts"`
	Type            string         `json:"type"`
	Session         string         `json:"session"`
	Data            map[string]any `json:"data"`
}

// Event type names. Changing the meaning of a name requires a new name.
const (
	TypeSessionCreated    = "session.created"
	TypeSessionState      = "session.state"
	TypeSessionCompleted  = "session.completed"
	TypeSessionFailed     = "session.failed"
	TypeSessionCancelled  = "session.cancelled"
	TypePlanComputed      = "plan.computed"
	TypePlanRejected      = "plan.rejected"
	TypeCheckFired        = "check.fired"
	TypePhaseStarted      = "phase.started"
	TypePhaseProgress     = "phase.progress"
	TypePhaseCompleted    = "phase.completed"
	TypePhaseFailed       = "phase.failed"
	TypeApprovalRequested = "approval.requested"
	TypeApprovalGranted   = "approval.granted"
	TypeApprovalDenied    = "approval.denied"
	TypeEngineStarted     = "engine.started"
	TypeEngineLog         = "engine.log"
	TypeEngineExited      = "engine.exited"
	TypeThrottleEngaged   = "throttle.engaged"
	TypeThrottleReleased  = "throttle.released"
	TypeCutoverAvailable  = "cutover.available"
	TypeCutoverDone       = "cutover.done"
	TypeRollbackAvailable = "rollback.available"
	TypeRollbackDone      = "rollback.done"
)

// sensitiveKey matches keys whose values must never reach a log or an event.
var sensitiveKey = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|credential|api[_-]?key|private[_-]?key|dsn)`)

// dsnPassword matches password=... or user:password@ inside connection strings.
var dsnPassword = regexp.MustCompile(`(?i)(password\s*=\s*)[^ &;]+|(:)([^:/@\s]+)(@)`)

// Redacted is the replacement text for removed values.
const Redacted = "[REDACTED]"

// Redact returns a copy of data with credentials removed. It recurses into maps
// and slices so a nested error payload cannot leak a password either.
func Redact(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		if sensitiveKey.MatchString(k) {
			out[k] = Redacted
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return Redact(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = redactValue(item)
		}
		return out
	case string:
		return RedactString(t)
	default:
		return v
	}
}

// RedactString scrubs credentials that appear inline in free text such as an
// engine command line or a MySQL error message.
func RedactString(s string) string {
	if s == "" {
		return s
	}
	s = dsnPassword.ReplaceAllStringFunc(s, func(m string) string {
		lower := strings.ToLower(m)
		if strings.HasPrefix(lower, "password") {
			i := strings.IndexByte(m, '=')
			if i < 0 {
				return Redacted
			}
			return m[:i+1] + Redacted
		}
		// user:password@ -> user:[REDACTED]@
		return ":" + Redacted + "@"
	})
	return s
}

// Writer emits sequenced, redacted events as JSONL.
//
// It is safe for concurrent use. Progress events are rate limited to at most one
// per second per session.
type Writer struct {
	mu           sync.Mutex
	w            io.Writer
	session      string
	seq          int64
	now          func() time.Time
	lastProgress map[string]time.Time
	minInterval  time.Duration
}

// NewWriter creates a writer bound to one session.
func NewWriter(w io.Writer, session string) *Writer {
	return &Writer{
		w:            w,
		session:      session,
		now:          time.Now,
		lastProgress: map[string]time.Time{},
		minInterval:  time.Second,
	}
}

// WithClock injects a clock, for tests.
func (wr *Writer) WithClock(now func() time.Time) *Writer {
	wr.now = now
	return wr
}

// Seq returns the last emitted sequence number.
func (wr *Writer) Seq() int64 {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	return wr.seq
}

// Emit writes one event. It returns the event that was written, or a zero Event
// when the event was suppressed by rate limiting.
func (wr *Writer) Emit(typ string, data map[string]any) (Event, error) {
	wr.mu.Lock()
	defer wr.mu.Unlock()

	now := wr.now()

	if typ == TypePhaseProgress {
		key := wr.session
		if phase, ok := data["phase"].(string); ok {
			key += "|" + phase
		}
		if last, ok := wr.lastProgress[key]; ok && now.Sub(last) < wr.minInterval {
			return Event{}, nil
		}
		wr.lastProgress[key] = now
	}

	wr.seq++
	ev := Event{
		ContractVersion: ContractVersion,
		Seq:             wr.seq,
		TS:              float64(now.UnixNano()) / 1e9,
		Type:            typ,
		Session:         wr.session,
		Data:            Redact(data),
	}

	enc := json.NewEncoder(wr.w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return ev, err
	}
	return ev, nil
}
