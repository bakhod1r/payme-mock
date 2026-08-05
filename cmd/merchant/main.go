// Command merchant serves the billing side Payme calls: the Merchant API
// webhook a cash register is pointed at.
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

	billinginfra "github.com/bakhod1r/payme-mock/internal/context/payment/billing/infrastructure"
	merchantinfra "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/infrastructure"
	accessinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/access/infrastructure"
	faultinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/infrastructure"
	sandboxinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/infrastructure"
	trafficdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	trafficinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/config"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
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
		log.Error("merchant stopped", "error", err)
		exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load[config.Merchant]()
	if err != nil {
		return err
	}

	pool, err := postgres.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.Database.MigrateOnStart {
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
	}

	clk := clock.New()

	handlers := newHandlerCache(pool, dependencies{
		transactions: merchantinfra.NewTransactionRepository(pool),
		events:       merchantinfra.NewEventRecorder(pool),
		accounts:     billinginfra.NewAccountRepository(pool),
		orders:       billinginfra.NewOrderRepository(pool),
		walkIns:      billinginfra.NewWalkInRepository(pool),
	}, clk)

	rules := faultinfra.NewRuleStore(pool)

	handler := withMiddleware(handlers, middlewareDeps{
		rules:   rules,
		hits:    rules,
		traffic: trafficinfra.NewRecorder(pool),
		clock:   clk,
	}, log)

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: routes(
			sandboxinfra.NewRepository(pool),
			httpx.Allowlist(accessinfra.NewRepository(pool), cfg.TrustForwardedFor, log),
			handler,
		),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The listener runs in its own goroutine so main can wait on whichever
	// comes first: a failed start or a shutdown signal.
	errs := make(chan error, 1)
	go func() {
		log.Info("merchant listening", "addr", cfg.HTTPAddr)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	return server.Shutdown(shutdownCtx) //nolint:contextcheck // see above
}

// trafficRecorder is the slice of the traffic log this service writes to.
type trafficRecorder = trafficdomain.Recorder

// routes serves the webhook under a per-sandbox path, which is the address the
// console shows and the gateway forwards to.
func routes(sandboxes sandboxLookup, guard func(http.Handler) http.Handler, handler http.Handler) http.Handler {
	mux := http.NewServeMux()

	// Every stand-scoped route is guarded: the address check runs after the
	// stand is known and before the handler, which is where the real provider
	// drops traffic from an address the merchant never registered.
	handler = guard(handler)

	mux.Handle("/s/{slug}/payme/merchant", resolveSandbox(sandboxes)(handler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}
