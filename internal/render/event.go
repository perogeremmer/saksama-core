package render

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/events"
)

// EventLine turns one event into the single sentence an operator needs.
//
// The mapping lives here rather than in the CLI because every viewer needs the
// same words: a terminal, a wizard progress pane, and a web activity feed that
// disagrees about what `phase.failed` means is three products, not one.
//
// An empty return means "nothing worth showing": approval and throttle events are
// noise in a scrolling log, and a viewer that prints them trains people to ignore
// the log.
func EventLine(ev events.Event) string {
	get := func(k string) string {
		v, ok := ev.Data[k]
		if !ok {
			return ""
		}
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}

	switch ev.Type {
	case events.TypeSessionCreated:
		return fmt.Sprintf("session %s created for %s.%s", ev.Session, get("database"), get("table"))
	case events.TypePlanComputed:
		return fmt.Sprintf("plan: engine %s, %s blocker(s)", get("engine"), get("blockers"))
	case events.TypePlanRejected:
		return fmt.Sprintf("plan REFUSED: %s blocker(s) %s", get("blockers"), get("reasons"))
	case events.TypePhaseStarted:
		return fmt.Sprintf("phase %s started", get("phase"))
	case events.TypePhaseProgress:
		return fmt.Sprintf("phase %s: %s%% %s", get("phase"), TrimFloat(get("percent")), get("note"))
	case events.TypePhaseCompleted:
		return fmt.Sprintf("phase %s done", get("phase"))
	case events.TypePhaseFailed:
		return fmt.Sprintf("phase %s FAILED: %s", get("phase"), get("error"))
	case events.TypeEngineStarted:
		return fmt.Sprintf("engine %s started (pid %s) %s", get("engine"), get("pid"), get("note"))
	case events.TypeEngineExited:
		return fmt.Sprintf("engine exited (code %s)", get("exit_code"))
	case events.TypeCheckFired:
		line := fmt.Sprintf("[%s] %s %s", get("severity"), get("id"), get("title"))
		if r := get("remediation"); r != "" {
			line += "\n    fix: " + r
		}
		return line
	case events.TypeSessionCompleted:
		return fmt.Sprintf("session %s completed, %s statement(s), %s verified", ev.Session, get("statements"), get("verified"))
	case events.TypeSessionFailed:
		return fmt.Sprintf("session %s FAILED: %s", ev.Session, get("error"))
	case events.TypeSessionCancelled:
		return fmt.Sprintf("session %s cancelled: %s", ev.Session, get("reason"))
	case events.TypeSessionState:
		return fmt.Sprintf("session %s: %s %s", ev.Session, get("state"), get("note"))
	case events.TypeApprovalGranted:
		return fmt.Sprintf("approved by %s: %s", get("actor"), get("note"))
	case events.TypeApprovalDenied:
		return fmt.Sprintf("approval denied by %s", get("actor"))
	case events.TypeThrottleEngaged:
		return fmt.Sprintf("throttled: %s", get("reason"))
	case events.TypeThrottleReleased:
		return "throttle released"
	case events.TypeCutoverAvailable:
		return fmt.Sprintf("cutover is ready and held; release it with: saksama cutover %s", ev.Session)
	case events.TypeCutoverDone:
		return "cutover done: the copy table is now the real table"
	case events.TypeRollbackAvailable:
		return "rollback boundary recorded; see the session for what is still recoverable"
	case events.TypeRollbackDone:
		return "rollback restored the previous table"
	}
	return ""
}

// TrimFloat keeps a percentage readable without rounding it into a lie: 12.34
// stays 12.3, not 12.
func TrimFloat(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 && len(s) > i+2 {
		return s[:i+2]
	}
	return s
}
