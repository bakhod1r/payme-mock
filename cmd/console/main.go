// Command console serves the control plane: the UI that creates sandboxes,
// switches configuration profiles and shows the traffic the stand has seen.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// exit ends the process. It is a variable so a test can run main itself and
// see what it decided, rather than leaving the entry point as the one path
// nothing covers.
var exit = osExit

// osExit is what exit is when nobody has replaced it, kept apart so a test can
// put it back.
var osExit = os.Exit

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// The signal context is cancelled on Ctrl-C or a container stop, which is
	// what starts the graceful shutdown inside run. It is made here rather than
	// inside run so a test can hand run a context of its own and watch it stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log); err != nil {
		log.Error("console stopped", "error", err)
		exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := postgres.Migrate(ctx, pool); err != nil {
		return err
	}

	store := &store{pool: pool}

	// Seeding is idempotent: a restart against a populated database leaves the
	// existing profiles alone.
	seeded, err := store.SeedProfiles(ctx)
	if err != nil {
		return err
	}
	if seeded > 0 {
		log.Info("seeded configuration profiles", "count", seeded)
	}

	errorsSeeded, err := store.SeedErrorCatalog(ctx)
	if err != nil {
		return err
	}
	if errorsSeeded > 0 {
		log.Info("seeded error catalog", "count", errorsSeeded)
	}

	app, err := newApp(cfg, store, log)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The listener runs in its own goroutine so main can wait on whichever
	// comes first: a failed start or a shutdown signal.
	errs := make(chan error, 1)
	go func() {
		log.Info("console listening", "addr", cfg.HTTPAddr, "user", cfg.Username)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Deliberately not derived from ctx: ctx being cancelled is what got us
	// here, and a shutdown context inheriting that cancellation would give
	// every in-flight request zero time to finish instead of ten seconds.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return server.Shutdown(shutdownCtx) //nolint:contextcheck // see above
}
