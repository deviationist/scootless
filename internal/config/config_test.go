package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadReadsEnvFile(t *testing.T) {
	dir := writeEnv(t, `
# a comment
SCOOTLESS_LAT=59.9139
SCOOTLESS_LON=10.7522
SCOOTLESS_RADIUS=250
SCOOTLESS_THRESHOLD=2
SCOOTLESS_OPERATOR=ryde,voi
SCOOTLESS_CLIENT_NAME="my-client"
export SCOOTLESS_INTERVAL=30s
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasHome || cfg.Home.Lat != 59.9139 || cfg.Home.Lon != 10.7522 {
		t.Errorf("Home = %+v (has=%v)", cfg.Home, cfg.HasHome)
	}
	if cfg.RadiusM != 250 || cfg.Threshold != 2 {
		t.Errorf("radius=%d threshold=%d", cfg.RadiusM, cfg.Threshold)
	}
	if len(cfg.OperatorKeys) != 2 || cfg.OperatorKeys[0] != "ryde" {
		t.Errorf("OperatorKeys = %v", cfg.OperatorKeys)
	}
	if cfg.ClientName != "my-client" {
		t.Errorf("ClientName = %q, quotes should be stripped", cfg.ClientName)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %v", cfg.Interval)
	}
}

func TestEnvironmentBeatsFile(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_RADIUS=100\n")
	t.Setenv("SCOOTLESS_RADIUS", "400")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RadiusM != 400 {
		t.Errorf("RadiusM = %d, want the environment to win", cfg.RadiusM)
	}
}

func TestMissingEnvFileIsNotAnError(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("a missing .env should be fine: %v", err)
	}
	if cfg.HasHome {
		t.Error("HasHome true with no configuration")
	}
	if cfg.RadiusM != 150 {
		t.Errorf("RadiusM = %d, want the default", cfg.RadiusM)
	}
}

// Half a coordinate is a typo, not a configuration, and silently ignoring it
// would mean reporting confidently on the wrong place.
func TestHalfACoordinateIsAnError(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_LAT=59.9139\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("want an error when only LAT is set")
	}
	dir = writeEnv(t, "SCOOTLESS_LON=10.7522\n")
	if _, err := Load(dir); err == nil {
		t.Fatal("want an error when only LON is set")
	}
}

func TestRejectsOutOfRangeCoordinates(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_LAT=91\nSCOOTLESS_LON=10\n")
	if _, err := Load(dir); err == nil {
		t.Error("want an error for a latitude above 90")
	}
}

func TestOperatorAllMeansNoRestriction(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_OPERATOR=all\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OperatorKeys != nil {
		t.Errorf("OperatorKeys = %v, want nil for \"all\"", cfg.OperatorKeys)
	}
}

// Polling faster than the data changes buys nothing and is rude to a free
// public dataset, so the floor is enforced rather than documented.
func TestRejectsAnAbsurdlyShortInterval(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_INTERVAL=1s\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Errorf("err = %v, want a floor on the interval", err)
	}
}

func TestRejectsBadValues(t *testing.T) {
	for _, body := range []string{
		"SCOOTLESS_RADIUS=banana\n",
		"SCOOTLESS_RADIUS=0\n",
		"SCOOTLESS_INTERVAL=soon\n",
		"SCOOTLESS_LAT=x\nSCOOTLESS_LON=10\n",
	} {
		if _, err := Load(writeEnv(t, body)); err == nil {
			t.Errorf("want an error for %q", strings.TrimSpace(body))
		}
	}
}

// A server with no topic sends nowhere while looking configured, which is the
// worst failure mode a notifier has.
func TestNtfyServerAndTopicMustBeSetTogether(t *testing.T) {
	if _, err := Load(writeEnv(t, "SCOOTLESS_NTFY_SERVER=https://ntfy.sh\n")); err == nil {
		t.Error("want an error for a server with no topic")
	}
	if _, err := Load(writeEnv(t, "SCOOTLESS_NTFY_TOPIC=abc\n")); err == nil {
		t.Error("want an error for a topic with no server")
	}
	cfg, err := Load(writeEnv(t,
		"SCOOTLESS_NTFY_SERVER=https://ntfy.sh\nSCOOTLESS_NTFY_TOPIC=abc\n"))
	if err != nil {
		t.Fatalf("both set should be valid: %v", err)
	}
	if cfg.NtfyTopic != "abc" {
		t.Errorf("NtfyTopic = %q", cfg.NtfyTopic)
	}
}

func TestNtfyPriorityIsRangeChecked(t *testing.T) {
	body := "SCOOTLESS_NTFY_SERVER=https://ntfy.sh\nSCOOTLESS_NTFY_TOPIC=abc\nSCOOTLESS_NTFY_PRIORITY=9\n"
	if _, err := Load(writeEnv(t, body)); err == nil {
		t.Error("want an error for a priority above 5")
	}
}
