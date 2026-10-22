package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestJSONFormatEmitsStructuredRecords(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "info", "json")

	l.Info("trip created", "trip_id", "t-1")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "trip created" || rec["trip_id"] != "t-1" || rec["level"] != "INFO" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestLevelFiltersLowerSeverities(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "warn", "text")

	l.Info("quiet")
	l.Warn("loud")

	out := buf.String()
	if strings.Contains(out, "quiet") || !strings.Contains(out, "loud") {
		t.Errorf("level filtering wrong:\n%s", out)
	}
}

func TestUnknownLevelAndFormatFallBackToDefaults(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "nonsense", "nonsense")

	l.Debug("hidden")
	l.Info("shown")

	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Errorf("defaults should be info/text:\n%s", buf.String())
	}
}

func TestSetupRoutesTheStandardLogPackageThroughSlog(t *testing.T) {
	var buf bytes.Buffer
	install(newLogger(&buf, "info", "json"))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	log.Printf("legacy line %d", 7)

	if !strings.Contains(buf.String(), "legacy line 7") || !strings.Contains(buf.String(), `"level"`) {
		t.Errorf("log.Printf did not reach slog:\n%s", buf.String())
	}
}
