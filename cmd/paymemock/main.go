// Command paymemock serves the provider side: the Subscribe API a backend
// calls, and the caller of the merchant's Merchant API. Nothing it returns
// says it is a mock.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	billinginfra "github.com/bakhod1r/payme-mock/internal/context/payment/billing/infrastructure"
	subscribedomain "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	subscribeinfra "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
	accessinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/access/infrastructure"
	faultinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/infrastructure"
	sandboxinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/infrastructure"
	trafficinfra "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/config"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// merchantCallTimeout bounds one Merchant API call. A merchant that never
// answers must not hold a receipt open forever; the provider gives up too.
const merchantCallTimeout = 30 * time.Second

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
		log.Error("paymemock stopped", "error", err)
		exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load[config.PaymeMock]()
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
	scheduler := subscribeinfra.NewScheduler(log)

	handlers := newHandlerCache(pool, dependencies{
		cards:     subscribeinfra.NewCardRepository(pool),
		receipts:  subscribeinfra.NewReceiptRepository(pool),
		scheduler: scheduler,
		tokens:    subscribeinfra.NewTokens(),
		sms:       subscribeinfra.NewSMSLog(log),
		newClient: merchantClients(),
		ledger:    billinginfra.NewCashboxLedger(pool),
	}, clk, cfg.MerchantBaseURL)

	rules := faultinfra.NewRuleStore(pool)

	handler := withMiddleware(handlers, middlewareDeps{
		rules:   rules,
		hits:    rules,
		traffic: trafficinfra.NewRecorder(pool),
		clock:   clk,
		repeats: httpx.NewIdempotencyMiddleware(
			postgres.NewCallStore(pool),
			idempotentMethods,
			cfg.IdempotencyWindow,
			func(ctx context.Context) (int64, bool) {
				sandbox, ok := sandboxctx.Get(ctx)
				return sandbox.ID, ok
			},
		),
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

	// The listener runs in its own goroutine so run can wait on whichever
	// comes first: a failed start or a shutdown signal.
	errs := make(chan error, 1)
	go func() {
		log.Info("paymemock listening", "addr", cfg.HTTPAddr)
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

	if err := server.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // see above
		return err
	}

	// Receipts mid-walk finish before the process exits, so a stand is never
	// left with a receipt frozen between states.
	scheduler.Wait()

	return nil
}

// routes serves the Subscribe API under a per-sandbox path, which is the
// address the console shows and the gateway forwards to, and under a shared
// path where the stand is named by the credential instead.
func routes(sandboxes sandboxLookup, guard func(http.Handler) http.Handler, handler http.Handler) http.Handler {
	mux := http.NewServeMux()

	// Every stand-scoped route is guarded: the address check runs after the
	// stand is known and before the handler, which is where the real provider
	// drops traffic from an address the merchant never registered.
	handler = guard(handler)

	mux.Handle("/s/{slug}/api", resolveSandbox(sandboxes)(handler))

	// A client that holds several cash registers but only one API URL — the
	// shape of the real provider's configuration — reaches its stands here,
	// named by the merchant id in X-Auth rather than by the path.
	mux.Handle("/api", resolveSandboxByAuth(sandboxes)(handler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

// merchantClients builds a Merchant API client per stand, all sharing one
// connection pool and one sequence of call identifiers.
func merchantClients() merchantClientFactory {
	client := &http.Client{Timeout: merchantCallTimeout}

	var counter atomic.Int64
	nextID := func() int64 { return counter.Add(1) }

	return func(endpoint, key string) subscribedomain.MerchantClient {
		return subscribeinfra.NewMerchantClient(endpoint, key, client, nextID)
	}
}
