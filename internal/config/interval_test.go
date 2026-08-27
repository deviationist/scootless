package config

import (
	"strings"
	"testing"

	"github.com/deviationist/scootless/internal/poll"
)

// The shipped default must be the poller's own, or the daemon runs at one
// cadence while the code documents another.
func TestDefaultIntervalTracksThePoller(t *testing.T) {
	if got := Default().Interval; got != poll.DefaultInterval {
		t.Errorf("Default().Interval = %v, want poll.DefaultInterval (%v)",
			got, poll.DefaultInterval)
	}
}

// The floor is the politeness limit on a free public dataset.
func TestIntervalBelowTheFloorIsRejected(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_LAT=59.9\nSCOOTLESS_LON=10.7\nSCOOTLESS_INTERVAL=1s\n")
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want an error for an interval below the floor")
	}
	if !strings.Contains(err.Error(), poll.MinInterval.String()) {
		t.Errorf("err = %v, want it to name the floor %v", err, poll.MinInterval)
	}
}

// Exactly the floor is allowed - it is a floor, not an exclusive bound.
func TestIntervalAtTheFloorIsAccepted(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_LAT=59.9\nSCOOTLESS_LON=10.7\nSCOOTLESS_INTERVAL="+
		poll.MinInterval.String()+"\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("the floor itself must be accepted: %v", err)
	}
	if cfg.Interval != poll.MinInterval {
		t.Errorf("Interval = %v, want %v", cfg.Interval, poll.MinInterval)
	}
}

// A faster interval than the default is the supported way to trade requests
// for latency, so it must not be quietly clamped back.
func TestAFasterIntervalIsHonoured(t *testing.T) {
	dir := writeEnv(t, "SCOOTLESS_LAT=59.9\nSCOOTLESS_LON=10.7\nSCOOTLESS_INTERVAL=6s\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval.Seconds() != 6 {
		t.Errorf("Interval = %v, want 6s honoured as configured", cfg.Interval)
	}
}
