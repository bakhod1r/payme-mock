// Package config loads every service's settings from the environment and .env
// files through oneenv, so the whole stand is configured one way.
//
// Only what a process needs to start lives here: addresses, credentials and
// connection strings. Everything that shapes the stand's behaviour — delays,
// injected errors, timeouts — belongs to a configuration profile in the
// database, so the console can change it without a restart.
package config

import (
	"fmt"
	"time"

	"github.com/bakhod1r/oneenv"
)

// Merchant is what cmd/merchant needs: the billing side Payme calls.
type Merchant struct {
	Database         Database
	Redis            Redis
	HTTPAddr         string `env:"HTTP_ADDR" default:":8081" desc:"listen address for the Merchant API"`
	SubscribeBaseURL string `env:"SUBSCRIBE_BASE_URL" default:"http://paymemock:8082/api" desc:"Subscribe API endpoint; point at checkout.test.paycom.uz to use the real provider"`
	// TrustForwardedFor makes the address checks read X-Forwarded-For instead
	// of the peer. It is on because both services normally sit behind the
	// gateway or behind Docker, where the peer is a proxy; exposed directly it
	// must be off, or a caller could name any address it likes.
	TrustForwardedFor bool          `env:"TRUST_FORWARDED_FOR" default:"true" desc:"read the client address from X-Forwarded-For; only safe behind a proxy you control"`
	ShutdownTimeout   time.Duration `env:"SHUTDOWN_TIMEOUT" default:"15s" desc:"how long to let in-flight requests finish"`
	LogLevel          string        `env:"LOG_LEVEL" default:"info" desc:"debug, info, warn or error"`
}

// PaymeMock is what cmd/paymemock needs: the provider emulator.
type PaymeMock struct {
	Database        Database
	Redis           Redis
	HTTPAddr        string `env:"HTTP_ADDR" default:":8082" desc:"listen address for the Subscribe API and checkout"`
	MerchantBaseURL string `env:"MERCHANT_BASE_URL" default:"http://merchant:8081" desc:"where to send Merchant API webhooks"`
	CheckoutBaseURL string `env:"CHECKOUT_BASE_URL" default:"http://localhost:8082" desc:"base of generated checkout links"`
	// TrustForwardedFor makes the address checks read X-Forwarded-For instead
	// of the peer. It is on because both services normally sit behind the
	// gateway or behind Docker, where the peer is a proxy; exposed directly it
	// must be off, or a caller could name any address it likes.
	TrustForwardedFor bool `env:"TRUST_FORWARDED_FOR" default:"true" desc:"read the client address from X-Forwarded-For; only safe behind a proxy you control"`
	// IdempotencyWindow is how long a call that created something is answered
	// out of its own earlier response.
	//
	// The Subscribe API carries no idempotency field, so a payout asked for
	// twice is two payouts and a client that lost the first response cannot
	// tell. The stand recognises the repeat by the JSON-RPC id the caller
	// already sends per intention. Zero turns it off, which is the provider's
	// own behaviour and what a rehearsal of duplicate payouts needs.
	IdempotencyWindow time.Duration `env:"IDEMPOTENCY_WINDOW" default:"24h" desc:"how long a repeated write call is answered with its first response; 0 to let repeats through"`
	ShutdownTimeout   time.Duration `env:"SHUTDOWN_TIMEOUT" default:"15s"`
	LogLevel          string        `env:"LOG_LEVEL" default:"info"`
}

// Console is what cmd/console needs: the control API and UI.
type Console struct {
	Database        Database
	Redis           Redis
	HTTPAddr        string        `env:"HTTP_ADDR" default:":8080" desc:"listen address for the control API and UI"`
	GatewayBaseURL  string        `env:"GATEWAY_BASE_URL" default:"https://merchant.localhost:8443" desc:"base used to show each sandbox its endpoint URL"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" default:"15s"`
	LogLevel        string        `env:"LOG_LEVEL" default:"info"`
}

// Worker is what cmd/worker needs: the background state walks.
type Worker struct {
	Database    Database
	Redis       Redis
	Concurrency int    `env:"WORKER_CONCURRENCY" default:"10" desc:"how many background steps run at once"`
	LogLevel    string `env:"LOG_LEVEL" default:"info"`
}

// Gateway is what cmd/gateway needs: the HTTPS front door.
type Gateway struct {
	Database          Database
	HTTPSAddr         string `env:"HTTPS_ADDR" default:":8443"`
	Mode              string `env:"GATEWAY_MODE" default:"local" desc:"local, exposed, prod or behind_proxy"`
	MerchantUpstream  string `env:"MERCHANT_UPSTREAM" default:"http://merchant:8081"`
	PaymeMockUpstream string `env:"PAYMEMOCK_UPSTREAM" default:"http://paymemock:8082"`
	CertDir           string `env:"CERT_DIR" default:"/certs" desc:"where the local CA and certificates are kept"`
	// Domains are the hostnames to obtain Let's Encrypt certificates for.
	// Required in prod mode and ignored otherwise.
	Domains  []string `env:"GATEWAY_DOMAINS" separator:","`
	ACMEMail string   `env:"ACME_EMAIL" desc:"contact address for Let's Encrypt"`
	LogLevel string   `env:"LOG_LEVEL" default:"info"`
}

// The gateway modes, matching what the plan describes.
const (
	// ModeLocal serves a self-signed certificate from its own CA.
	ModeLocal = "local"
	// ModeExposed is local TLS behind a public address or tunnel.
	ModeExposed = "exposed"
	// ModeProd obtains real certificates from Let's Encrypt.
	ModeProd = "prod"
	// ModeBehindProxy serves plain HTTP because something else terminates TLS.
	ModeBehindProxy = "behind_proxy"
)

// Database is how a service reaches PostgreSQL.
//
// The keys are spelled in full rather than taking a prefix, so every service
// and docker-compose name them the same way.
type Database struct {
	URL string `env:"DATABASE_URL" default:"postgres://payme:payme@localhost:5433/paymemock?sslmode=disable" desc:"PostgreSQL connection string" secret:"true"`
	// MigrateOnStart brings the schema up before serving. Convenient for the
	// stand; a deployment that migrates separately turns it off.
	MigrateOnStart bool `env:"DB_MIGRATE_ON_START" default:"true" desc:"run migrations during startup"`
}

// Redis is how a service reaches Redis, which backs the background queue.
type Redis struct {
	Addr     string `env:"REDIS_ADDR" default:"localhost:6380" desc:"Redis address for the background queue"`
	Password string `env:"REDIS_PASSWORD" secret:"true"`
	DB       int    `env:"REDIS_DB" default:"0"`
}

// Load reads a service's configuration from the environment and any .env files.
//
// With no arguments it uses the environment-aware cascade: `.env`, then
// `.env.local`, then the files for the active APP_ENV. Every one of them is
// optional, because in a container the whole configuration arrives as
// environment variables and no .env file exists at all.
//
// Naming files explicitly is for tests and for deployments that keep their
// settings somewhere specific; those files must exist.
func Load[T any](files ...string) (*T, error) {
	opts := []oneenv.Option{oneenv.WithEnvFiles()}
	if len(files) > 0 {
		opts = []oneenv.Option{oneenv.WithFiles(files...)}
	}

	cfg, err := oneenv.Parse[T](opts...)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}

	return cfg, nil
}

// ValidateGatewayMode reports an unknown gateway mode, which would otherwise
// silently fall back to serving no TLS at all.
func ValidateGatewayMode(mode string) error {
	switch mode {
	case ModeLocal, ModeExposed, ModeProd, ModeBehindProxy:
		return nil
	default:
		return fmt.Errorf("unknown gateway mode %q: expected %s, %s, %s or %s",
			mode, ModeLocal, ModeExposed, ModeProd, ModeBehindProxy)
	}
}
