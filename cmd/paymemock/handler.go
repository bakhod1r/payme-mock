package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/application"
	subscribedomain "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
	subscribehttp "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/interfaces/http"
	configdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/config/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// handlerCache builds one Subscribe API handler per stand and profile.
//
// The credentials and the merchant endpoint differ per stand, and the timings
// come from the profile in force, so a switch of either must take effect
// without a restart; caching keeps that from rebuilding on every request.
type handlerCache struct {
	pool  *postgres.Pool
	deps  dependencies
	clock clock.Clock
	// merchantBase is where a stand's Merchant API lives. The stand's slug is
	// appended, which is the address the console shows.
	merchantBase string

	mu    sync.RWMutex
	built map[cacheKey]http.Handler
}

// cacheKey names a handler: one stand under one profile.
type cacheKey struct {
	sandboxID int64
	configID  int64
}

// dependencies are the ports every handler is wired to. The repositories hold
// only a pool and scope themselves through the request context.
type dependencies struct {
	cards     subscribedomain.CardRepository
	receipts  subscribedomain.ReceiptRepository
	scheduler *infrastructure.Scheduler
	tokens    subscribedomain.TokenGenerator
	sms       subscribedomain.SMSSender
	newClient merchantClientFactory
	// ledger writes the register's transaction and moves its balance for a
	// payout, neither of which a Merchant API chain does: a payout is the
	// provider paying a card, not asking a merchant for anything.
	ledger subscribedomain.CashboxLedger
}

// merchantClientFactory builds the client the Payme side calls a merchant with.
// It is a function so a test can substitute a client that never leaves the
// process.
type merchantClientFactory func(endpoint, key string) subscribedomain.MerchantClient

func newHandlerCache(pool *postgres.Pool, deps dependencies, clk clock.Clock, merchantBase string) *handlerCache {
	return &handlerCache{
		pool:         pool,
		deps:         deps,
		clock:        clk,
		merchantBase: merchantBase,
		built:        make(map[cacheKey]http.Handler),
	}
}

// ServeHTTP resolves the stand and profile in force and delegates to their
// handler.
func (c *handlerCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandbox, ok := sandboxctx.Get(r.Context())
	if !ok {
		// Reaching here unscoped is a wiring mistake, not a caller's error.
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	handler, err := c.forSandbox(r.Context(), sandbox)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	handler.ServeHTTP(w, r)
}

func (c *handlerCache) forSandbox(ctx context.Context, sandbox sandboxctx.Sandbox) (http.Handler, error) {
	key := cacheKey{sandboxID: sandbox.ID, configID: sandbox.ConfigID}

	c.mu.RLock()
	handler, ok := c.built[key]
	c.mu.RUnlock()
	if ok {
		return handler, nil
	}

	settings, err := c.loadSettings(ctx, sandbox.ConfigID)
	if err != nil {
		return nil, err
	}

	service := application.NewService(
		c.deps.cards, c.deps.receipts,
		c.deps.newClient(c.merchantEndpoint(sandbox.Slug), sandbox.Key),
		c.deps.scheduler, c.deps.tokens, c.deps.sms, c.clock,
		application.Settings{
			SandboxID:        sandbox.ID,
			MerchantID:       sandbox.MerchantID,
			MerchantName:     sandbox.MerchantName,
			VerifyCode:       settings.CardVerifyCode,
			VerifyWaitMillis: settings.CardVerifyWaitMillis,
			StepDelayMillis:  settings.StepDelayMillis,
			HoldWindowMillis: settings.HoldWindowMillis,
			CardBalance:      settings.CardBalance,
		},
		application.WithLedger(c.deps.ledger),
	)

	// The scheduler runs the state walk, and the walk is a call back into the
	// service that queued it. Attaching it here is what closes that loop.
	c.deps.scheduler.SetAdvance(service.AdvanceReceipt)

	handler = subscribehttp.NewHandler(service, resolveCredentials)

	c.mu.Lock()
	defer c.mu.Unlock()
	// Another request may have built the same handler meanwhile. The two are
	// equivalent — same stand, same profile, same ports — so the last one in is
	// kept and both callers work from a handler that behaves identically.
	c.built[key] = handler

	return handler, nil
}

// merchantEndpoint is the Merchant API address of one stand, which is the same
// URL the console shows and a cash register would be configured with.
func (c *handlerCache) merchantEndpoint(slug string) string {
	return strings.TrimSuffix(c.merchantBase, "/") + "/s/" + slug + "/payme/merchant"
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
