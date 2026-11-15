package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseCallbackRoundTrip(t *testing.T) {
	kb := MainKeyboard("HDBCD-1")
	for _, btn := range kb[0] {
		if _, err := ParseCallback(btn.CallbackData); err != nil {
			t.Errorf("%q: %v", btn.CallbackData, err)
		}
	}
	snz := SnoozeKeyboard("HDBCD-1", []time.Duration{30 * time.Minute, 24 * time.Hour})
	cb, err := ParseCallback(snz[0][1].CallbackData)
	if err != nil || cb.Action != ActSnooze || cb.Key != "HDBCD-1" || cb.Snooze != 24*time.Hour {
		t.Fatalf("snooze cb = %+v, %v", cb, err)
	}
	if snz[0][0].Text != "30m" || snz[0][1].Text != "1d" {
		t.Errorf("labels = %q %q", snz[0][0].Text, snz[0][1].Text)
	}
	for _, bad := range []string{"", "fix", "fix:", "zzz:X-1", "snz:X-1:abc", "snz:X-1:-1m", "fix:X-1:extra"} {
		if _, err := ParseCallback(bad); err == nil {
			t.Errorf("ParseCallback(%q) expected error", bad)
		}
	}
	for _, btn := range append(kb[0], snz[0]...) {
		if len(btn.CallbackData) > 64 {
			t.Errorf("callback data too long: %q", btn.CallbackData)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{30 * time.Minute: "30m", 2 * time.Hour: "2h", 90 * time.Minute: "1h30m", 48 * time.Hour: "2d"}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRenderNoticeEscapes(t *testing.T) {
	got := RenderNotice(Notice{Key: "X-1", Summary: "a <b> & c", URL: "https://j/browse/X-1"}, "", true)
	if !strings.Contains(got, "a &lt;b&gt; &amp; c") || !strings.HasPrefix(got, "🐞 <s>") {
		t.Errorf("render = %q", got)
	}
}

func TestClientSendAndErrors(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":42,"chat":{"id":7}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "TOKEN")
	id, err := c.SendMessage(context.Background(), 7, "hi", MainKeyboard("X-1"), 5)
	if err != nil || id != 42 {
		t.Fatalf("SendMessage = %d, %v", id, err)
	}
	if gotPath != "/botTOKEN/sendMessage" || gotBody["parse_mode"] != "HTML" || gotBody["reply_parameters"] == nil {
		t.Errorf("request: path=%s body=%v", gotPath, gotBody)
	}

	err = c.EditMessage(context.Background(), 7, 42, "x", nil)
	if !IsNotModified(err) {
		t.Errorf("expected not-modified error, got %v", err)
	}
}

func TestJobKeyboardRetryOnly(t *testing.T) {
	kb := JobKeyboard("HDBCD-2700")
	if len(kb) != 1 || len(kb[0]) != 1 || kb[0][0].CallbackData != "rty:HDBCD-2700" {
		t.Fatalf("kb = %+v", kb)
	}
	// Buttons of the removed repo picker (old messages) are rejected.
	for _, bad := range []string{"repo:X-1:0", "orp:X-1"} {
		if _, err := ParseCallback(bad); err == nil {
			t.Errorf("ParseCallback(%q) expected error", bad)
		}
	}
}
