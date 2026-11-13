package telegram

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Callback actions encoded in button callback_data (≤64 bytes).
const (
	ActFix        = "fix"  // fix:<KEY>
	ActSnoozeMenu = "snz"  // snz:<KEY>            → show duration choices
	ActSnooze     = "snz"  // snz:<KEY>:<duration> → snooze
	ActBack       = "back" // back:<KEY>           → main keyboard
)

// Callback is a parsed callback_data.
type Callback struct {
	Action string
	Key    string
	Snooze time.Duration // set only for snz:<KEY>:<d>
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
		cb.Action == ActSnoozeMenu && len(parts) == 2:
		return cb, nil
	case cb.Action == ActSnooze && len(parts) == 3:
		d, err := time.ParseDuration(parts[2])
		if err != nil || d <= 0 {
			return Callback{}, fmt.Errorf("bad snooze duration in %q", data)
		}
		cb.Snooze = d
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
