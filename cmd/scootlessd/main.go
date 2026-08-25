// Command scootlessd polls for nearby rentable scooters, records what it sees,
// and notifies armed watches when one appears.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/deviationist/scootless/internal/config"
	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/poll"
	"github.com/deviationist/scootless/internal/store"
)

// homeFenceID is the fence built from configuration. Once the app can supply a
// position, this becomes one fence among several rather than the only one.
const homeFenceID = "home"

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "scootlessd:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir     = flag.String("config", ".", "directory to read .env from")
		dbPath  = flag.String("db", "", "override the database path")
		once    = flag.Bool("once", false, "run a single tick and exit")
		verbose = flag.Bool("v", false, "log debug detail, including coordinates")
	)
	flag.Parse()

	cfg, err := config.Load(*dir)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if !cfg.HasHome {
		return errors.New("no location configured: set SCOOTLESS_LAT and SCOOTLESS_LON " +
			"in .env (copy .env.example to start)")
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return fmt.Errorf("creating database directory: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fence := store.Fence{
		ID: homeFenceID, Name: homeFenceID, At: cfg.Home, RadiusM: cfg.RadiusM,
	}
	if err := st.SaveFence(ctx, fence); err != nil {
		return fmt.Errorf("saving fence: %w", err)
	}

	// Coordinates are logged only under -v. They are the one genuinely
	// personal value here, and a daemon's log tends to outlive the intent
	// behind it.
	log.Info("starting",
		"db", cfg.DBPath,
		"fence", fence.Name,
		"radius_m", fence.RadiusM,
		"interval", cfg.Interval,
		"operators", operatorLabel(cfg.OperatorKeys))
	log.Debug("fence position", "lat", cfg.Home.Lat, "lon", cfg.Home.Lon)

	p := &poll.Poller{
		Client:   entur.New(cfg.ClientName),
		Store:    st,
		Interval: cfg.Interval,
		Log:      log,
	}

	if *once {
		rep, err := p.Tick(ctx, time.Now())
		if err != nil {
			return err
		}
		logTick(log, rep)
		return nil
	}

	log.Info("polling; press ctrl-c to stop")
	err = p.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("stopped")
		return nil
	}
	return err
}

func logTick(log *slog.Logger, rep poll.Report) {
	log.Info("tick",
		"queries", rep.Queries,
		"fences", rep.Fences,
		"arrivals", rep.Arrivals,
		"expired", rep.Expired,
		"fired", len(rep.Fired),
		"truncated", rep.Truncated)
}

func operatorLabel(keys []string) string {
	if len(keys) == 0 {
		return "all"
	}
	s := keys[0]
	for _, k := range keys[1:] {
		s += "," + k
	}
	return s
}
