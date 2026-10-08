package oslog

import (
	"strings"
	"testing"
	"time"
)

// A real line captured from `log stream --style ndjson` on an iOS simulator.
const sampleLine = `{"timezoneName":"","messageType":"Info","eventType":"logEvent","source":null,"formatString":"{\"msg%{public}.0s\":\"Nowhere to go\"}","userID":502,"activityIdentifier":0,"subsystem":"com.apple.locationd.Core","category":"Simulation","threadID":27039148,"bootUUID":"","processImagePath":"\/usr\/libexec\/locationd","timestamp":"2026-10-08 14:17:12.175004+0700","eventMessage":"{\"msg\":\"Nowhere to go\"}","processID":53893,"traceID":102832147143983364}`

func TestParseEvent(t *testing.T) {
	entry, ok := ParseEvent([]byte(sampleLine), "iPhone 17")
	if !ok {
		t.Fatal("expected the ndjson line to parse")
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"level", entry.Level, "Info"},
		{"lvl", entry.Level, "Info"},
		{"subsystem", entry.Subsystem, "com.apple.locationd.Core"},
		{"category", entry.Category, "Simulation"},
		{"process", entry.Process, "locationd"},
		{"processID", entry.ProcessID, 53893},
		{"threadID", entry.ThreadID, 27039148},
		{"message", entry.Message, `{"msg":"Nowhere to go"}`},
		{"eventType", entry.EventType, "logEvent"},
		{"source", entry.Source, "iPhone 17"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	want := time.Date(2026, 10, 8, 14, 17, 12, 175004000, time.FixedZone("", 7*3600))
	if !entry.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", entry.Timestamp, want)
	}
}

func TestParseEventIgnoresNonJSON(t *testing.T) {
	for _, line := range []string{
		"",
		"   ",
		`Filtering the log data using "subsystem == \"com.apple.CoreAnalytics\""`,
		"getpwuid_r did not find a match for uid 502",
		`{"timestamp":`,
	} {
		if entry, ok := ParseEvent([]byte(line), "src"); ok {
			t.Errorf("ParseEvent(%q) unexpectedly parsed to %+v", line, entry)
		}
	}
}

func TestParseEventTimestampVariants(t *testing.T) {
	for _, ts := range []string{
		"2026-10-08 14:17:12.175004+0700",
		"2026-10-08 14:17:12.175+0700",
		"2026-10-08 14:17:12+0700",
	} {
		line := `{"timestamp":"` + ts + `","eventMessage":"hi"}`
		entry, ok := ParseEvent([]byte(line), "src")
		if !ok {
			t.Fatalf("failed to parse timestamp %q", ts)
		}
		if entry.Timestamp.Year() != 2026 || entry.Timestamp.Hour() != 14 {
			t.Errorf("timestamp %q parsed as %v", ts, entry.Timestamp)
		}
	}
}

func TestEventLevelFallbacks(t *testing.T) {
	cases := map[string]string{
		`{"eventType":"signpostEvent","signpostName":"load"}`:                  "Signpost",
		`{"eventType":"lossEvent","messageType":"","eventMessage":"lost 3"}`:   "Loss",
		`{"eventType":"activityCreateEvent"}`:                                  "Activity",
		`{"eventType":"timesyncEvent"}`:                                        "Timesync",
		`{"eventType":"stateEvent"}`:                                           "State",
		`{"eventType":"logEvent"}`:                                             "Default",
		`{"eventType":"logEvent","messageType":"Error","eventMessage":"boom"}`: "Error",
	}
	for line, want := range cases {
		entry, ok := ParseEvent([]byte(line), "src")
		if !ok {
			t.Fatalf("failed to parse %s", line)
		}
		if entry.Level != want {
			t.Errorf("level for %s = %q, want %q", line, entry.Level, want)
		}
	}
}

func TestSignpostCaptured(t *testing.T) {
	entry, ok := ParseEvent([]byte(`{"eventType":"signpostEvent","signpostName":"decode","signpostType":"begin","signpostScope":"process"}`), "src")
	if !ok {
		t.Fatal("expected parse")
	}
	if entry.Signpost == nil || entry.Signpost.Name != "decode" || entry.Signpost.Type != "begin" {
		t.Fatalf("signpost = %+v", entry.Signpost)
	}
}

func TestProcessNameFallback(t *testing.T) {
	if got := processName("", "/System/Library/Foo.app/Foo"); got != "Foo" {
		t.Errorf("processName fallback = %q, want Foo", got)
	}
	if got := processName("", ""); got != "unknown" {
		t.Errorf("processName with no paths = %q, want unknown", got)
	}
}

func TestMessageFallbackToFormatString(t *testing.T) {
	entry, ok := ParseEvent([]byte(`{"formatString":"plain template"}`), "src")
	if !ok {
		t.Fatal("expected parse")
	}
	if entry.Message != "plain template" {
		t.Errorf("message = %q, want the format string", entry.Message)
	}
}

func TestPrettyRuntime(t *testing.T) {
	cases := map[string]string{
		"com.apple.CoreSimulator.SimRuntime.iOS-26-5":       "iOS 26.5",
		"com.apple.CoreSimulator.SimRuntime.watchOS-11-6-1": "watchOS 11.6.1",
		"iOS-17-5": "iOS 17.5",
	}
	for in, want := range cases {
		if got := prettyRuntime(in); got != want {
			t.Errorf("prettyRuntime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb"); got != "a" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine = %q", got)
	}
}

func TestFatalForNotice(t *testing.T) {
	cases := []struct {
		line  string
		fatal bool
		want  string
	}{
		{"log: Must be admin to run 'stream' command", true, "sudo"},
		{"Process spawn via launchd failed because device is not booted.", true, "boot the simulator"},
		{"Bad or unknown session: com.apple.CoreSimulator.SimDevice.X", true, "not available"},
		{`Filtering the log data using "subsystem == \"com.a\""`, false, ""},
	}
	for _, c := range cases {
		err := fatalForNotice(c.line)
		if c.fatal {
			if err == nil {
				t.Errorf("fatalForNotice(%q) = nil, want a fatal error", c.line)
				continue
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("fatalForNotice(%q) = %q, want it to mention %q", c.line, err, c.want)
			}
			if !isFatal(err) {
				t.Errorf("fatalForNotice(%q) did not return a fatal error", c.line)
			}
		} else if err != nil {
			t.Errorf("fatalForNotice(%q) = %v, want nil", c.line, err)
		}
	}
}

func TestBenignNotices(t *testing.T) {
	for _, line := range []string{
		"getpwuid_r did not find a match for uid 502",
		"Child process terminated with signal 15: Terminated",
	} {
		if !isBenignNotice(line) {
			t.Errorf("isBenignNotice(%q) = false, want true", line)
		}
	}
	if isBenignNotice("real error") {
		t.Error("isBenignNotice must not swallow real output")
	}
}

func TestBuildCommand(t *testing.T) {
	sim := NewStreamer(Options{Target: Target{Kind: KindSimulator, ID: "UDID-1"}, Level: "debug", Predicate: `subsystem == "com.a"`})
	name, args := sim.buildCommand()
	if name != "xcrun" {
		t.Errorf("simulator command = %q, want xcrun", name)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"simctl spawn UDID-1 log stream", "--style ndjson", "--level debug", `--predicate subsystem == "com.a"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("simulator args %q missing %q", joined, want)
		}
	}

	dev := NewStreamer(Options{Target: Target{Kind: KindDevice, ID: "UDID-2"}, Level: ""})
	name, args = dev.buildCommand()
	if name != "log" {
		t.Errorf("device command = %q, want log", name)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--device-udid UDID-2") {
		t.Errorf("device args %q missing --device-udid", joined)
	}
	if !strings.Contains(joined, "--level default") {
		t.Errorf("device args %q should default the level", joined)
	}
	if strings.Contains(joined, "--predicate") {
		t.Errorf("device args %q must not add an empty predicate", joined)
	}
}

func TestValidLevel(t *testing.T) {
	for _, level := range []string{"default", "info", "debug", "DEBUG"} {
		if !ValidLevel(level) {
			t.Errorf("ValidLevel(%q) = false", level)
		}
	}
	for _, level := range []string{"", "verbose", "error"} {
		if ValidLevel(level) {
			t.Errorf("ValidLevel(%q) = true", level)
		}
	}
}
