package telegram

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// Callback actions encoded in button callback_data (≤64 bytes).
const (
	ActFix        = "fix"  // fix:<KEY>
	ActSnoozeMenu = "snz"  // snz:<KEY>            → show duration choices
	ActSnooze     = "snz"  // snz:<KEY>:<duration> → snooze
	ActBack       = "back" // back:<KEY>           → main keyboard
	ActRepo       = "repo" // repo:<KEY>:<index>   → approve + queue agent on that repo
	ActRetry      = "rty"  // rty:<KEY>            → re-run the agent on the last job's repo
	ActOtherRepo  = "orp"  // orp:<KEY>            → pick another repo for a new job

	// Handover callbacks carry the handover id in Key.
	ActHandoverYes     = "hoy" // hoy:<id> → create + approve + merge dev → test MRs
	ActHandoverLater   = "hol" // hol:<id> → ask again later
	ActHandoverRewatch = "hor" // hor:<id> → watch CI/CD again after a retry in GitLab
)

// Callback is a parsed callback_data.
type Callback struct {
	Action    string
	Key       string
	Snooze    time.Duration // set only for snz:<KEY>:<d>
	RepoIndex int           // set only for repo:<KEY>:<i>
}

// ParseCallback decodes callback_data produced by this package.
func ParseCallback(data string) (Callback, error) {
	parts := strings.Split(data, ":")
	if len(parts) < 2 || parts[1] == "" {
		return Callback{}, fmt.Errorf("bad callback %q", data)
	}
	cb := Callback{Action: parts[0], Key: parts[1]}
	switch {
	case cb.Action == ActFix && len(parts) == 2, cb.Action == ActBack && len(parts) == 2,
		cb.Action == ActSnoozeMenu && len(parts) == 2,
		cb.Action == ActRetry && len(parts) == 2, cb.Action == ActOtherRepo && len(parts) == 2,
		cb.Action == ActHandoverYes && len(parts) == 2, cb.Action == ActHandoverLater && len(parts) == 2,
		cb.Action == ActHandoverRewatch && len(parts) == 2:
		return cb, nil
	case cb.Action == ActSnooze && len(parts) == 3:
		d, err := time.ParseDuration(parts[2])
		if err != nil || d <= 0 {
			return Callback{}, fmt.Errorf("bad snooze duration in %q", data)
		}
		cb.Snooze = d
		return cb, nil
	case cb.Action == ActRepo && len(parts) == 3:
		i, err := strconv.Atoi(parts[2])
		if err != nil || i < 0 {
			return Callback{}, fmt.Errorf("bad repo index in %q", data)
		}
		cb.RepoIndex = i
		return cb, nil
	}
	return Callback{}, fmt.Errorf("unknown callback %q", data)
}

// MainKeyboard is shown on an open notice.
func MainKeyboard(key string) Keyboard {
	return Keyboard{{
		{Text: "🛠 Fix it", CallbackData: ActFix + ":" + key},
		{Text: "⏰ Remind me after…", CallbackData: ActSnoozeMenu + ":" + key},
	}}
}

// SnoozeKeyboard lists duration choices plus a back button.
func SnoozeKeyboard(key string, choices []time.Duration) Keyboard {
	row := make([]InlineButton, 0, len(choices))
	for _, d := range choices {
		row = append(row, InlineButton{Text: FormatDuration(d), CallbackData: ActSnooze + ":" + key + ":" + d.String()})
	}
	return Keyboard{row, {{Text: "« Back", CallbackData: ActBack + ":" + key}}}
}

// RepoKeyboard lets the user pick which repo the agent should work on (index-based
// callback data keeps it under 64 bytes regardless of repo name length).
func RepoKeyboard(key string, names []string) Keyboard {
	kb := make(Keyboard, 0, len(names)+1)
	for i, n := range names {
		kb = append(kb, []InlineButton{{Text: "📦 " + n, CallbackData: fmt.Sprintf("%s:%s:%d", ActRepo, key, i)}})
	}
	return append(kb, []InlineButton{{Text: "« Back", CallbackData: ActBack + ":" + key}})
}

// JobKeyboard is shown on an approved notice once its job has finished.
func JobKeyboard(key string) Keyboard {
	return Keyboard{{
		{Text: "🔁 Retry", CallbackData: ActRetry + ":" + key},
		{Text: "📦 Other repo", CallbackData: ActOtherRepo + ":" + key},
	}}
}

// HandoverKeyboard asks whether to merge dev → test and hand over now.
func HandoverKeyboard(id int64) Keyboard {
	sid := strconv.FormatInt(id, 10)
	return Keyboard{{
		{Text: "🚀 Có, merge sang test", CallbackData: ActHandoverYes + ":" + sid},
		{Text: "⏰ Để sau", CallbackData: ActHandoverLater + ":" + sid},
	}}
}

// RewatchKeyboard is shown on a failed CI/CD report.
func RewatchKeyboard(id int64) Keyboard {
	return Keyboard{{{Text: "🔁 Theo dõi lại CI/CD", CallbackData: ActHandoverRewatch + ":" + strconv.FormatInt(id, 10)}}}
}

// FooterMerged: the fix is in the shared dev branch, waiting for the next handover.
func FooterMerged(repo, mrURL string) string {
	return "🔀 <b>Merged vào dev</b> (<code>" + html.EscapeString(repo) + `</code>, <a href="` + html.EscapeString(mrURL) +
		`">MR</a>) — chờ bàn giao`
}

// FooterDelivered: the handover CI/CD succeeded.
func FooterDelivered(repo string) string {
	return "🚀 <b>Đã bàn giao test</b> (<code>" + html.EscapeString(repo) + "</code>)"
}

// Job footers: progress of the agent run on an approved task.
func FooterNoRepo() string {
	return "✅ <b>Approved</b> — ⚠️ no repo configured for this project (repos.json)"
}
func FooterQueued(repo string) string {
	return "🕓 <b>Queued</b> — agent will work on <code>" + html.EscapeString(repo) + "</code>"
}
func FooterRetrying(repo string, attempt int, at string, resume bool) string {
	s := fmt.Sprintf("🔁 <b>Auto-retry #%d</b> on <code>%s</code> at %s", attempt, html.EscapeString(repo), html.EscapeString(at))
	if resume {
		s += " — continuing the partial work"
	}
	return s
}
func FooterRunning(repo string) string {
	return "🤖 <b>Agent running</b> on <code>" + html.EscapeString(repo) + "</code>…"
}
func FooterFailed(repo, reason string) string {
	return "❌ <b>Agent failed</b> on <code>" + html.EscapeString(repo) + "</code>: " + html.EscapeString(reason)
}
func FooterDone(repo, mrURL, branch string, testsPassed *bool) string {
	tests := "tests not run"
	if testsPassed != nil {
		tests = "tests ✅"
		if !*testsPassed {
			tests = "tests ❌"
		}
	}
	target := "branch <code>" + html.EscapeString(branch) + "</code> (not pushed)"
	if mrURL != "" {
		target = `<a href="` + html.EscapeString(mrURL) + `">Draft MR</a>`
	}
	return "✅ <b>Done</b> on <code>" + html.EscapeString(repo) + "</code>: " + target + " · " + tests
}

// Notice is what a task message displays.
type Notice struct {
	Key, Summary, Priority, Status, URL string
}

// Footer variants appended under the notice body.
func FooterApproved(at time.Time) string {
	return "✅ <b>Approved</b> " + at.Format("15:04 02/01") + " — queued for agent"
}
func FooterSnoozed(until time.Time) string {
	return "⏰ Snoozed until <b>" + until.Format("15:04 02/01") + "</b>"
}
func FooterClosed() string   { return "🔒 No longer open/assigned in Jira" }
func FooterReminder() string { return "🔔 Reminder — still waiting for your decision" }

// FooterSuperseded marks an older notice whose buttons moved to a newer reminder.
func FooterSuperseded() string { return "↪ Reminder sent below" }

// RenderNotice formats a task as Telegram HTML. strike crosses out the title (closed tasks).
func RenderNotice(n Notice, footer string, strike bool) string {
	title := fmt.Sprintf("<b>[%s]</b> %s", html.EscapeString(n.Key), html.EscapeString(n.Summary))
	if strike {
		title = "<s>" + title + "</s>"
	}
	var b strings.Builder
	b.WriteString("🐞 " + title + "\n")
	var meta []string
	if n.Priority != "" {
		meta = append(meta, "Priority: "+html.EscapeString(n.Priority))
	}
	if n.Status != "" {
		meta = append(meta, "Status: "+html.EscapeString(n.Status))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n")
	}
	fmt.Fprintf(&b, `<a href="%s">Open in Jira</a>`, html.EscapeString(n.URL))
	if footer != "" {
		b.WriteString("\n\n" + footer)
	}
	return b.String()
}

// FormatDuration renders 30m / 2h / 1d / 1h30m for button labels.
func FormatDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	s := d.String() // e.g. 1h30m0s, 2h0m0s
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
