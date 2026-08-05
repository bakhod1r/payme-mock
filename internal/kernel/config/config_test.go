package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/config"
)

// writeEnv creates a .env cascade in a temporary directory and makes it the
// working directory, since Load reads relative paths.
func writeEnv(t *testing.T, files map[string]string) {
	t.Helper()

	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}

	original, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(original) })
}

func TestLoadAppliesDefaults(t *testing.T) {
	writeEnv(t, map[string]string{".env": ""})

	got, err := config.Load[config.Merchant]()

	require.NoError(t, err)
	assert.Equal(t, ":8081", got.HTTPAddr)
	assert.Equal(t, 15*time.Second, got.ShutdownTimeout)
	assert.Equal(t, "info", got.LogLevel)
	assert.True(t, got.Database.MigrateOnStart)
	assert.Equal(t, "localhost:6380", got.Redis.Addr)
}

func TestLoadReadsTheEnvFile(t *testing.T) {
	writeEnv(t, map[string]string{".env": `
HTTP_ADDR=:9090
LOG_LEVEL=debug
SHUTDOWN_TIMEOUT=30s
SUBSCRIBE_BASE_URL=https://checkout.test.paycom.uz/api
`})

	got, err := config.Load[config.Merchant]()

	require.NoError(t, err)
	assert.Equal(t, ":9090", got.HTTPAddr)
	assert.Equal(t, "debug", got.LogLevel)
	assert.Equal(t, 30*time.Second, got.ShutdownTimeout)
	assert.Equal(t, "https://checkout.test.paycom.uz/api", got.SubscribeBaseURL)
}

// The database and Redis blocks are nested structs but their keys are spelled
// in full, so docker-compose and every service name them the same way.
func TestLoadReadsNestedSettings(t *testing.T) {
	writeEnv(t, map[string]string{".env": `
DATABASE_URL=postgres://user:pass@db:5432/paymemock?sslmode=disable
DB_MIGRATE_ON_START=false
REDIS_ADDR=redis:6379
REDIS_DB=3
`})

	got, err := config.Load[config.Merchant]()

	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@db:5432/paymemock?sslmode=disable", got.Database.URL)
	assert.False(t, got.Database.MigrateOnStart)
	assert.Equal(t, "redis:6379", got.Redis.Addr)
	assert.Equal(t, 3, got.Redis.DB)
}

// A later file wins, which is what lets .env.local hold personal overrides
// without editing the checked-in defaults.
func TestLocalFileOverridesTheSharedOne(t *testing.T) {
	writeEnv(t, map[string]string{
		".env":       "HTTP_ADDR=:8081\nLOG_LEVEL=info\n",
		".env.local": "HTTP_ADDR=:7777\n",
	})

	got, err := config.Load[config.Merchant]()

	require.NoError(t, err)
	assert.Equal(t, ":7777", got.HTTPAddr, "the local file wins")
	assert.Equal(t, "info", got.LogLevel, "keys it does not mention are untouched")
}

func TestLoadAcceptsExplicitFiles(t *testing.T) {
	writeEnv(t, map[string]string{
		".env":         "HTTP_ADDR=:8081\n",
		".env.staging": "HTTP_ADDR=:6000\n",
	})

	got, err := config.Load[config.Merchant](".env", ".env.staging")

	require.NoError(t, err)
	assert.Equal(t, ":6000", got.HTTPAddr)
}

func TestLoadReportsAnUnreadableValue(t *testing.T) {
	writeEnv(t, map[string]string{".env": "SHUTDOWN_TIMEOUT=not-a-duration\n"})

	_, err := config.Load[config.Merchant]()

	assert.ErrorContains(t, err, "load configuration")
}

// Naming a file explicitly means it must be there: a deployment pointed at a
// settings file that has gone missing should stop, not start on defaults.
func TestLoadReportsAMissingExplicitFile(t *testing.T) {
	writeEnv(t, nil)

	_, err := config.Load[config.Merchant](".env.production")

	assert.ErrorContains(t, err, "load configuration")
}

// In a container the whole configuration arrives as environment variables and
// no .env file exists, so a bare environment must still start every service.
func TestLoadWithoutAnyEnvFile(t *testing.T) {
	writeEnv(t, nil)

	tests := []struct {
		name string
		load func() error
	}{
		{"merchant", func() error { _, err := config.Load[config.Merchant](); return err }},
		{"paymemock", func() error { _, err := config.Load[config.PaymeMock](); return err }},
		{"console", func() error { _, err := config.Load[config.Console](); return err }},
		{"worker", func() error { _, err := config.Load[config.Worker](); return err }},
		{"gateway", func() error { _, err := config.Load[config.Gateway](); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, tt.load())
		})
	}
}

func TestServiceDefaultsDoNotCollide(t *testing.T) {
	writeEnv(t, nil)

	merchant, err := config.Load[config.Merchant]()
	require.NoError(t, err)
	paymemock, err := config.Load[config.PaymeMock]()
	require.NoError(t, err)
	console, err := config.Load[config.Console]()
	require.NoError(t, err)
	gateway, err := config.Load[config.Gateway]()
	require.NoError(t, err)

	addrs := []string{merchant.HTTPAddr, paymemock.HTTPAddr, console.HTTPAddr, gateway.HTTPSAddr}
	seen := make(map[string]bool, len(addrs))
	for _, addr := range addrs {
		assert.False(t, seen[addr], "two services default to %s and would fight for the port", addr)
		seen[addr] = true
	}
}

func TestGatewayDomainsAreCommaSeparated(t *testing.T) {
	writeEnv(t, map[string]string{".env": "GATEWAY_DOMAINS=pay.example.uz,api.example.uz\n"})

	got, err := config.Load[config.Gateway]()

	require.NoError(t, err)
	assert.Equal(t, []string{"pay.example.uz", "api.example.uz"}, got.Domains)
}

func TestWorkerConcurrencyDefault(t *testing.T) {
	writeEnv(t, nil)

	got, err := config.Load[config.Worker]()

	require.NoError(t, err)
	assert.Equal(t, 10, got.Concurrency)
}

func TestValidateGatewayMode(t *testing.T) {
	accepted := []string{config.ModeLocal, config.ModeExposed, config.ModeProd, config.ModeBehindProxy}
	for _, mode := range accepted {
		t.Run("accepts "+mode, func(t *testing.T) {
			assert.NoError(t, config.ValidateGatewayMode(mode))
		})
	}

	// An unknown mode must stop the gateway rather than let it fall through to
	// serving no TLS at all.
	for _, mode := range []string{"", "https", "LOCAL", "production"} {
		t.Run("rejects "+mode, func(t *testing.T) {
			err := config.ValidateGatewayMode(mode)

			require.Error(t, err)
			assert.ErrorContains(t, err, "unknown gateway mode")
		})
	}
}

func TestGatewayDefaultsToLocalMode(t *testing.T) {
	writeEnv(t, nil)

	got, err := config.Load[config.Gateway]()

	require.NoError(t, err)
	assert.Equal(t, config.ModeLocal, got.Mode)
	assert.NoError(t, config.ValidateGatewayMode(got.Mode))
}
