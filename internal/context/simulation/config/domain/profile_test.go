package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/config/domain"
	fault "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

func TestDefaultSettingsMatchTheDocumentedTimings(t *testing.T) {
	got := domain.DefaultSettings()

	assert.Equal(t, int64(43_200_000), got.TransactionTimeoutMillis, "twelve hours")
	assert.Equal(t, int64(60_000), got.CardVerifyWaitMillis, "the documented OTP window")
	assert.Equal(t, "666666", got.CardVerifyCode)
	assert.Equal(t, int64(30*24*60*60*1000), got.HoldWindowMillis, "thirty days")
	assert.Zero(t, got.DefaultDelayMillis, "a clean stand injects no delay")
	assert.NoError(t, got.Validate())
}

func TestSettingsValidate(t *testing.T) {
	rejects := []struct {
		name   string
		mutate func(*domain.Settings)
	}{
		{"a zero timeout", func(s *domain.Settings) { s.TransactionTimeoutMillis = 0 }},
		{"a negative timeout", func(s *domain.Settings) { s.TransactionTimeoutMillis = -1 }},
		{"no account field", func(s *domain.Settings) { s.AccountField = "" }},
		{"no verify code", func(s *domain.Settings) { s.CardVerifyCode = "" }},
		{"a negative step delay", func(s *domain.Settings) { s.StepDelayMillis = -1 }},
		{"a negative default delay", func(s *domain.Settings) { s.DefaultDelayMillis = -1 }},
		{"a zero page size", func(s *domain.Settings) { s.StatementPageSize = 0 }},
		{"an unknown reading of state 5", func(s *domain.Settings) { s.State5Meaning = "whatever" }},
	}

	for _, tt := range rejects {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			s := domain.DefaultSettings()
			tt.mutate(&s)

			assert.ErrorIs(t, s.Validate(), domain.ErrInvalid)
		})
	}

	t.Run("accepts both readings of state 5", func(t *testing.T) {
		for _, meaning := range []string{domain.State5Hold, domain.State5Archived} {
			s := domain.DefaultSettings()
			s.State5Meaning = meaning

			assert.NoError(t, s.Validate(), "the documentation supports %q", meaning)
		}
	})
}

func TestNewProfile(t *testing.T) {
	t.Run("normalises the name", func(t *testing.T) {
		got, err := domain.NewProfile("  Slow  ", "two minutes", domain.DefaultSettings())

		require.NoError(t, err)
		assert.Equal(t, "slow", got.Name)
		assert.False(t, got.Builtin, "a profile the user creates is not built in")
	})

	t.Run("rejects an empty name", func(t *testing.T) {
		_, err := domain.NewProfile("   ", "", domain.DefaultSettings())

		assert.ErrorIs(t, err, domain.ErrInvalid)
	})

	t.Run("rejects invalid settings", func(t *testing.T) {
		bad := domain.DefaultSettings()
		bad.StatementPageSize = 0

		_, err := domain.NewProfile("x", "", bad)

		assert.ErrorIs(t, err, domain.ErrInvalid)
	})
}

func TestBuiltinProfilesCannotBeDeleted(t *testing.T) {
	custom, err := domain.NewProfile("mine", "", domain.DefaultSettings())
	require.NoError(t, err)
	assert.True(t, custom.Deletable())

	for _, seed := range domain.SeedProfiles() {
		assert.False(t, seed.Profile.Deletable(), "%s is built in", seed.Profile.Name)
	}
}

// The seeded profiles are what the console offers out of the box.
func TestSeedProfiles(t *testing.T) {
	seeds := domain.SeedProfiles()

	var names []string
	for _, s := range seeds {
		names = append(names, s.Profile.Name)
	}

	assert.Equal(t, []string{
		domain.ProfileClean, domain.ProfileSlow, domain.ProfileFlaky,
		domain.ProfileErrors, domain.ProfileTimeout, domain.ProfileStrict,
		domain.ProfileWalkIn,
	}, names)
}

func TestEverySeedProfileIsValid(t *testing.T) {
	for _, seed := range domain.SeedProfiles() {
		t.Run(seed.Profile.Name, func(t *testing.T) {
			assert.NoError(t, seed.Profile.Settings.Validate())
			assert.True(t, seed.Profile.Builtin)
			assert.NotEmpty(t, seed.Profile.Description, "a profile the user picks needs an explanation")

			for _, rule := range seed.Rules {
				assert.True(t, rule.Action.Valid(), "rule %q has an unknown action", rule.Name)
				assert.True(t, rule.Enabled, "a seeded rule that is off does nothing")
				assert.GreaterOrEqual(t, rule.Probability, 0.0)
				assert.LessOrEqual(t, rule.Probability, 1.0)
			}
		})
	}
}

func TestCleanProfileInjectsNothing(t *testing.T) {
	clean := seedNamed(t, domain.ProfileClean)

	assert.Empty(t, clean.Rules, "the clean profile must leave traffic untouched")
}

// The console's headline case: switch to "slow" and every call takes two minutes.
func TestSlowProfileDelaysEveryCallByTwoMinutes(t *testing.T) {
	slow := seedNamed(t, domain.ProfileSlow)

	require.Len(t, slow.Rules, 1)
	rule := slow.Rules[0]
	assert.Equal(t, fault.ActionDelay, rule.Action)
	assert.Equal(t, 120_000, rule.DelayMillis)
	assert.Equal(t, "*", rule.Method)
	assert.Equal(t, fault.ServiceAny, rule.Service)
}

func TestFlakyProfileFiresOnlySometimes(t *testing.T) {
	flaky := seedNamed(t, domain.ProfileFlaky)

	require.NotEmpty(t, flaky.Rules)
	for _, rule := range flaky.Rules {
		assert.Less(t, rule.Probability, 1.0, "a flaky rule that always fires is not flaky")
		assert.Greater(t, rule.Probability, 0.0)
	}
}

func TestErrorsProfileCoversEveryMerchantMethod(t *testing.T) {
	errs := seedNamed(t, domain.ProfileErrors)

	covered := map[string]payerr.Code{}
	for _, rule := range errs.Rules {
		require.Equal(t, fault.ActionRPCError, rule.Action)
		covered[rule.Method] = rule.ErrorCode
	}

	for _, method := range []string{
		"CheckPerformTransaction", "CreateTransaction",
		"PerformTransaction", "CancelTransaction", "CheckTransaction",
	} {
		code, ok := covered[method]
		assert.True(t, ok, "%s has no error rule", method)
		_, documented := payerr.ByCode(code)
		assert.True(t, documented, "%s is assigned undocumented code %d", method, code)
	}
}

func TestTimeoutProfileDropsConnections(t *testing.T) {
	timeout := seedNamed(t, domain.ProfileTimeout)

	require.Len(t, timeout.Rules, 1)
	assert.Equal(t, fault.ActionDrop, timeout.Rules[0].Action)
}

// The strict profile shortens deadlines so mistakes surface in a test run
// rather than after twelve hours.
func TestStrictProfileShortensDeadlinesAndDuplicatesWrites(t *testing.T) {
	strict := seedNamed(t, domain.ProfileStrict)

	assert.Less(t, strict.Profile.Settings.TransactionTimeoutMillis,
		domain.DefaultSettings().TransactionTimeoutMillis)
	assert.Less(t, strict.Profile.Settings.CardVerifyWaitMillis,
		domain.DefaultSettings().CardVerifyWaitMillis)

	require.Len(t, strict.Rules, 1)
	assert.Equal(t, fault.ActionDuplicate, strict.Rules[0].Action)
}

func seedNamed(t *testing.T, name string) domain.Seed {
	t.Helper()

	for _, s := range domain.SeedProfiles() {
		if s.Profile.Name == name {
			return s
		}
	}

	t.Fatalf("no seeded profile named %q", name)
	return domain.Seed{}
}

// Only the walk-in profile accepts a payer the merchant has never seen. A
// stand is meant to refuse an unknown order unless it was asked not to.
func TestOnlyWalkInAutoRegistersAccounts(t *testing.T) {
	assert.False(t, domain.DefaultSettings().AutoRegisterAccounts)

	for _, seed := range domain.SeedProfiles() {
		want := seed.Profile.Name == domain.ProfileWalkIn
		assert.Equal(t, want, seed.Profile.Settings.AutoRegisterAccounts,
			"profile %s", seed.Profile.Name)
	}
}
