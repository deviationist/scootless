// Package config loads scootless's settings from a .env file and the
// environment, keeping personal values - coordinates above all - out of the
// repository and out of any committed file.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/poll"
)

// Prefix is the environment-variable namespace.
const Prefix = "SCOOTLESS_"

// Config is everything the daemon needs to start.
type Config struct {
	// Home is the default fence. It comes from configuration rather than code
	// so that a home address is never committed; once the app exists it will
	// also be supplied per request, from a saved place or the phone's own
	// position.
	Home      geo.Point
	HasHome   bool
	RadiusM   int
	Threshold int

	OperatorKeys []string
	ClientName   string

	DBPath   string
	Interval time.Duration
	HTTPAddr string

	// APIToken, when set, is required as a bearer token on the API. Empty
	// means the API is open, which is only appropriate on a loopback bind.
	APIToken string

	// AllowedOrigins are browser origins allowed to call the API
	// cross-origin, comma-separated in the environment. Empty means no CORS,
	// which is correct for a loopback-only deployment.
	AllowedOrigins []string

	NtfyServer   string
	NtfyTopic    string
	NtfyToken    string
	NtfyPriority int

	MQTTBroker   string
	MQTTTopic    string
	MQTTClientID string
	MQTTUsername string
	MQTTPassword string
}

// Default returns the configuration before any file or environment is read.
func Default() Config {
	return Config{
		RadiusM:      150,
		Threshold:    3,
		ClientName:   "scootless",
		DBPath:       defaultDBPath(),
		Interval:     poll.DefaultInterval,
		HTTPAddr:     "127.0.0.1:8099",
		MQTTTopic:    "scootless",
		MQTTClientID: "scootless",
	}
}

// Load reads .env from dir (if present), overlays SCOOTLESS_* environment
// variables, and returns the result. Environment wins over the file.
func Load(dir string) (Config, error) {
	cfg := Default()
	values, err := readEnvFile(filepath.Join(dir, ".env"))
	if err != nil {
		return cfg, err
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, Prefix) {
			values[k] = v
		}
	}
	if err := cfg.apply(values); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) apply(v map[string]string) error {
	lat, hasLat := v[Prefix+"LAT"]
	lon, hasLon := v[Prefix+"LON"]
	switch {
	case hasLat && hasLon:
		la, err := strconv.ParseFloat(strings.TrimSpace(lat), 64)
		if err != nil {
			return fmt.Errorf("%sLAT: %w", Prefix, err)
		}
		lo, err := strconv.ParseFloat(strings.TrimSpace(lon), 64)
		if err != nil {
			return fmt.Errorf("%sLON: %w", Prefix, err)
		}
		if la < -90 || la > 90 || lo < -180 || lo > 180 {
			return fmt.Errorf("%sLAT/%sLON out of range", Prefix, Prefix)
		}
		c.Home = geo.Point{Lat: la, Lon: lo}
		c.HasHome = true
	case hasLat != hasLon:
		// Half a coordinate is a typo, not a configuration. Saying so beats
		// silently falling back and reporting on the wrong place.
		return fmt.Errorf("%sLAT and %sLON must be set together", Prefix, Prefix)
	}

	if err := intVal(v, Prefix+"RADIUS", &c.RadiusM); err != nil {
		return err
	}
	if err := intVal(v, Prefix+"THRESHOLD", &c.Threshold); err != nil {
		return err
	}
	if s, ok := nonEmpty(v, Prefix+"OPERATOR"); ok {
		c.OperatorKeys = parseOperators(s)
	}
	strVal(v, Prefix+"CLIENT_NAME", &c.ClientName)
	strVal(v, Prefix+"DB", &c.DBPath)
	strVal(v, Prefix+"HTTP_ADDR", &c.HTTPAddr)
	strVal(v, Prefix+"API_TOKEN", &c.APIToken)
	if s, ok := nonEmpty(v, Prefix+"ALLOWED_ORIGINS"); ok {
		c.AllowedOrigins = parseOrigins(s)
	}
	strVal(v, Prefix+"NTFY_SERVER", &c.NtfyServer)
	strVal(v, Prefix+"NTFY_TOPIC", &c.NtfyTopic)
	strVal(v, Prefix+"NTFY_TOKEN", &c.NtfyToken)
	if err := intVal(v, Prefix+"NTFY_PRIORITY", &c.NtfyPriority); err != nil {
		return err
	}
	strVal(v, Prefix+"MQTT_BROKER", &c.MQTTBroker)
	strVal(v, Prefix+"MQTT_TOPIC", &c.MQTTTopic)
	strVal(v, Prefix+"MQTT_CLIENT_ID", &c.MQTTClientID)
	strVal(v, Prefix+"MQTT_USERNAME", &c.MQTTUsername)
	strVal(v, Prefix+"MQTT_PASSWORD", &c.MQTTPassword)

	if s, ok := nonEmpty(v, Prefix+"INTERVAL"); ok {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%sINTERVAL: %w", Prefix, err)
		}
		if d < poll.MinInterval {
			// Measured, the feed changes about every 15 s across all
			// operators. Below this we are sampling several times faster than
			// the data moves: it buys almost nothing and is discourteous to a
			// free public dataset.
			return fmt.Errorf("%sINTERVAL must be at least %s", Prefix, poll.MinInterval)
		}
		c.Interval = d
	}
	if c.RadiusM <= 0 {
		return fmt.Errorf("%sRADIUS must be positive", Prefix)
	}
	// A server without a topic silently sends nowhere, which is the worst
	// possible failure for a notifier: it looks configured and is not.
	if (c.NtfyServer == "") != (c.NtfyTopic == "") {
		return fmt.Errorf("%sNTFY_SERVER and %sNTFY_TOPIC must be set together", Prefix, Prefix)
	}
	if c.NtfyPriority < 0 || c.NtfyPriority > 5 {
		return fmt.Errorf("%sNTFY_PRIORITY must be between 1 and 5", Prefix)
	}
	return nil
}

// parseOperators reads a comma-separated list. "all" means every operator,
// which is represented as no restriction at all.
func parseOperators(s string) []string {
	if strings.EqualFold(strings.TrimSpace(s), "all") {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readEnvFile parses a .env file. A missing file is not an error: the
// environment alone is a valid way to configure this.
func readEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out, sc.Err()
}

func nonEmpty(v map[string]string, key string) (string, bool) {
	s, ok := v[key]
	s = strings.TrimSpace(s)
	return s, ok && s != ""
}

func strVal(v map[string]string, key string, dst *string) {
	if s, ok := nonEmpty(v, key); ok {
		*dst = s
	}
}

func intVal(v map[string]string, key string, dst *int) error {
	s, ok := nonEmpty(v, key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*dst = n
	return nil
}

func defaultDBPath() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "scootless", "scootless.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "scootless.db"
	}
	return filepath.Join(home, ".local", "state", "scootless", "scootless.db")
}

// parseOrigins splits a comma-separated origin list, trimming blanks. An
// origin is scheme + host + port with no trailing slash; a trailing slash is
// the usual way to configure this wrong, so it is stripped rather than
// silently failing to match.
func parseOrigins(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimRight(strings.TrimSpace(part), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}
