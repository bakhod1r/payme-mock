package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/application"
	merchantdomain "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	merchanthttp "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/interfaces/http"
	configdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/config/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// handlerCache builds one Merchant API handler per configuration profile and
// keeps it.
//
// The tunables a handler is built with come from the profile in force, so a
// profile switch in the console must take effect without a restart; caching by
// profile is what keeps that from rebuilding the router on every request.
type handlerCache struct {
	pool  *postgres.Pool
	deps  dependencies
	clock clock.Clock

	mu    sync.RWMutex
	built map[registerKey]http.Handler
}

// registerKey is what a built handler is keyed by: the profile it was built
// from and the direction the register moves money in.
type registerKey struct {
	configID int64
	kind     string
}

// dependencies are the ports every handler is wired to. They are shared: the
// repositories hold only a pool and scope themselves through the context.
type dependencies struct {
	transactions merchantdomain.TransactionRepository
	events       merchantdomain.EventRecorder
	accounts     billing.AccountRepository
	orders       billing.OrderRepository
	walkIns      billing.WalkInRepository
}

func newHandlerCache(pool *postgres.Pool, deps dependencies, clk clock.Clock) *handlerCache {
	return &handlerCache{
		pool:  pool,
		deps:  deps,
		clock: clk,
		built: make(map[registerKey]http.Handler),
	}
}

// ServeHTTP resolves the profile in force and delegates to its handler.
func (c *handlerCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := sandboxctx.Get(r.Context())
	if !ok {
		// Reaching here unscoped is a wiring mistake, not a caller's error.
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	handler, err := c.forRegister(r.Context(), sandbox.ConfigID, sandbox.Kind)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	handler.ServeHTTP(w, r)
}

// forRegister returns the handler for a profile and register kind, building it
// on first use.
//
// The kind joins the cache key because it decides which way a performed payment
// moves the balance, so two stands on the same profile still need their own
// handler when they face opposite directions.
func (c *handlerCache) forRegister(ctx context.Context, configID int64, kind string) (http.Handler, error) {
	key := registerKey{configID: configID, kind: kind}

	c.mu.RLock()
	handler, ok := c.built[key]
	c.mu.RUnlock()
	if ok {
		return handler, nil
	}

	settings, err := c.loadSettings(ctx, configID)
	if err != nil {
		return nil, err
	}

	service := application.NewService(
		c.deps.transactions, c.deps.events, c.deps.accounts, c.deps.orders,
		c.deps.walkIns, c.clock,
		application.Settings{
			TransactionTimeoutMillis: settings.TransactionTimeoutMillis,
			AccountField:             settings.AccountField,
			RegisterKind:             kind,
			AutoRegisterAccounts:     settings.AutoRegisterAccounts,
		},
	)

	handler = merchanthttp.NewHandler(service, resolveKey)

	c.mu.Lock()
	defer c.mu.Unlock()
	// Another request may have built the same profile while this one was
	// reading the database. The two are equivalent — same profile, same
	// direction, same ports — so the last one in is kept and both callers work
	// from a handler that behaves identically.
	c.built[key] = handler

	return handler, nil
}

// loadSettings reads a profile's tunables. A stand with no profile runs on the
// documented defaults rather than refusing to serve.
func (c *handlerCache) loadSettings(ctx context.Context, configID int64) (configdomain.Settings, error) {
	if configID == 0 {
		return configdomain.DefaultSettings(), nil
	}

	var raw []byte
	err := c.pool.QueryRow(ctx,
		`SELECT settings FROM control.configs WHERE id = $1`, configID).Scan(&raw)
	if err != nil {
		return configdomain.Settings{}, fmt.Errorf("load profile %d: %w", configID, err)
	}

	// Missing keys keep their default, so a profile saved by an older console
	// does not blank out settings it never knew about.
	settings := configdomain.DefaultSettings()
	if err := json.Unmarshal(raw, &settings); err != nil {
		return configdomain.Settings{}, fmt.Errorf("decode profile %d: %w", configID, err)
	}

	return settings, nil
}
