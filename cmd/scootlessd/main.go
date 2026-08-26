// Command scootlessd polls for nearby rentable scooters, records what it sees,
// and notifies armed watches when one appears.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/deviationist/scootless/internal/api"
	"github.com/deviationist/scootless/internal/config"
	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/notify"
	"github.com/deviationist/scootless/internal/poll"
	"github.com/deviationist/scootless/internal/store"
	"github.com/deviationist/scootless/internal/transit"
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
		noHTTP  = flag.Bool("no-http", false, "do not serve the API")
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

	client := entur.New(cfg.ClientName)
	sink, closeSink := buildSink(ctx, cfg, log)
	defer closeSink()

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
		Client:   client,
		Store:    st,
		Sink:     sink,
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

	if !*noHTTP {
		srv := &http.Server{
			Addr: cfg.HTTPAddr,
			Handler: (&api.Server{
				Store: st, Client: client, Log: log, Token: cfg.APIToken,
				Transit: transit.New(cfg.ClientName),
			}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Info("serving api", "addr", cfg.HTTPAddr, "authenticated", cfg.APIToken != "")
			if cfg.APIToken == "" {
				// Worth saying out loud rather than discovering later: an
				// unauthenticated API is only safe while it is bound to
				// loopback or a trusted interface.
				log.Warn("api has no token; keep it off any public interface " +
					"(set SCOOTLESS_API_TOKEN)")
			}
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("api server stopped", "err", err)
			}
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		}()
	}

	log.Info("polling; send SIGINT or SIGTERM to stop")
	err = p.Run(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("stopped")
		return nil
	}
	return err
}

// buildSink returns the notification sink and a function to release it. With
// no broker configured the watch machinery still works fully; it just logs,
// which keeps the feature usable before any messaging exists.
func buildSink(ctx context.Context, cfg config.Config, log *slog.Logger) (poll.Sink, func()) {
	var (
		sinks   notify.Multi
		closers []func()
	)

	if cfg.MQTTBroker != "" {
		m, err := notify.Dial(ctx, notify.Options{
			Broker: cfg.MQTTBroker, ClientID: cfg.MQTTClientID,
			Username: cfg.MQTTUsername, Password: cfg.MQTTPassword,
			Prefix: cfg.MQTTTopic, Log: log,
		})
		if err != nil {
			// A broker that is down must not stop the collector: the history
			// is still worth recording, the watch still fires and is
			// recorded, and any other sink still delivers.
			log.Error("connecting to mqtt; continuing without it", "err", err)
		} else {
			log.Info("publishing to mqtt", "topic_prefix", cfg.MQTTTopic)
			sinks = append(sinks, m)
			closers = append(closers, m.Close)
		}
	}

	if cfg.NtfyServer != "" {
		log.Info("publishing to ntfy", "server", cfg.NtfyServer,
			"authenticated", cfg.NtfyToken != "")
		sinks = append(sinks, &notify.Ntfy{
			Server: cfg.NtfyServer, Topic: cfg.NtfyTopic,
			Token: cfg.NtfyToken, Priority: cfg.NtfyPriority,
		})
	}

	release := func() {
		for _, c := range closers {
			c()
		}
	}
	if len(sinks) == 0 {
		log.Info("no notification sink configured; watches will be logged only")
		return notify.LogSink{Log: log}, release
	}
	// Always log as well, so a fired watch is visible in the journal even
	// when every delivery path is having a bad day.
	sinks = append(sinks, notify.LogSink{Log: log})
	return sinks, release
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
