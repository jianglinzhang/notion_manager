package proxy

import "testing"

func TestNextSkipsIneligibleAccounts(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "personal",
				UserEmail: "blocked@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
				},
			},
			{
				UserEmail: "eligible@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
				},
			},
		},
	}

	got := pool.Next()
	if got == nil || got.UserEmail != "eligible@example.com" {
		t.Fatalf("expected eligible account, got %#v", got)
	}
}

func TestGetBestAccountPrefersEligibleAccount(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "personal",
				UserEmail: "blocked@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
					SpaceLimit:     200,
					SpaceUsage:     180,
				},
			},
			{
				UserEmail: "eligible@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 20,
				},
			},
		},
	}

	got := pool.GetBestAccount()
	if got == nil || got.UserEmail != "eligible@example.com" {
		t.Fatalf("expected eligible account, got %#v", got)
	}
}

func TestGetBestAccountReturnsNilWhenOnlyIneligibleAccounts(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "business",
				UserEmail: "blocked@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
				},
			},
		},
	}

	if got := pool.GetBestAccount(); got != nil {
		t.Fatalf("expected nil, got %#v", got)
	}
}

func TestIsQuotaExhaustedUsesEligibilityFlag(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				PlanType:  "personal",
				UserEmail: "personal@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:     false,
					HasPremium:     true,
					PremiumBalance: 1300000,
					PremiumLimit:   1300000,
				},
			},
		},
	}

	if got := pool.isQuotaExhausted(pool.accounts[0]); !got {
		t.Fatalf("expected account with is_eligible=false to be exhausted")
	}
}

func TestGetBestAccountUsesEffectiveRemaining(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				UserEmail: "space-rich-user-poor@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 20,
					UserLimit:  200,
					UserUsage:  190,
				},
			},
			{
				UserEmail: "balanced@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 60,
					UserLimit:  200,
					UserUsage:  60,
				},
			},
		},
	}

	got := pool.GetBestAccount()
	if got == nil || got.UserEmail != "balanced@example.com" {
		t.Fatalf("expected account with higher effective remaining, got %#v", got)
	}
}

func TestNextRoundRobinsRegardlessOfQuota(t *testing.T) {
	// Next() should rotate through accounts regardless of remaining quota.
	pool := &AccountPool{
		accounts: []*Account{
			{
				UserEmail: "low@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 160,
					UserLimit:  200,
					UserUsage:  160,
				},
			},
			{
				UserEmail: "high@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible: true,
					SpaceLimit: 200,
					SpaceUsage: 40,
					UserLimit:  200,
					UserUsage:  40,
				},
			},
		},
	}

	first := pool.Next()
	second := pool.Next()
	if first == nil || second == nil {
		t.Fatal("expected non-nil accounts")
	}
	if first.UserEmail == second.UserEmail {
		t.Fatalf("expected different accounts on consecutive calls, both got %s", first.UserEmail)
	}
}

func TestNextExcludingRoundRobinsSkippingExcluded(t *testing.T) {
	low := &Account{
		UserEmail: "low@example.com",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			SpaceUsage: 170,
			UserLimit:  200,
			UserUsage:  170,
		},
	}
	mid := &Account{
		UserEmail: "mid@example.com",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			SpaceUsage: 80,
			UserLimit:  200,
			UserUsage:  80,
		},
	}
	high := &Account{
		UserEmail: "high@example.com",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			SpaceUsage: 20,
			UserLimit:  200,
			UserUsage:  20,
		},
	}
	pool := &AccountPool{
		accounts: []*Account{low, mid, high},
	}

	// With high excluded, NextExcluding should rotate between low and mid
	exclude := map[*Account]bool{high: true}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		got := pool.NextExcluding(exclude)
		if got == nil {
			t.Fatal("expected non-nil account")
		}
		if got.UserEmail == "high@example.com" {
			t.Fatal("excluded account should not be returned")
		}
		seen[got.UserEmail]++
	}
	if seen["low@example.com"] == 0 || seen["mid@example.com"] == 0 {
		t.Fatalf("expected both low and mid to appear, got: %v", seen)
	}
}

func TestNextForResearchAllowsPremiumAtLimit(t *testing.T) {
	pool := &AccountPool{
		accounts: []*Account{
			{
				UserEmail: "premium@example.com",
				QuotaInfo: &QuotaInfo{
					IsEligible:        true,
					HasPremium:        true,
					ResearchModeUsage: 3,
				},
			},
		},
	}

	got := pool.NextForResearch()
	if got == nil || got.UserEmail != "premium@example.com" {
		t.Fatalf("expected premium account to remain research-capable, got %#v", got)
	}
}

// ── Round-Robin distribution tests ──

func TestNextDistributesEvenly(t *testing.T) {
	// Three accounts with identical quota — Next() must rotate across all three.
	pool := &AccountPool{
		accounts: []*Account{
			{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 100, UserLimit: 200, UserUsage: 100}},
			{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 100, UserLimit: 200, UserUsage: 100}},
			{UserEmail: "c@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 100, UserLimit: 200, UserUsage: 100}},
		},
	}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		acc := pool.Next()
		if acc == nil {
			t.Fatal("expected non-nil account")
		}
		seen[acc.UserEmail]++
	}
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if seen[email] != 2 {
			t.Fatalf("expected each account called 2 times in 6 calls, got distribution: %v", seen)
		}
	}
}

func TestNextDistributesWithUnequalQuota(t *testing.T) {
	// Even with different remaining quotas, Next() should still round-robin.
	pool := &AccountPool{
		accounts: []*Account{
			{UserEmail: "low@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 160, UserLimit: 200, UserUsage: 160}},
			{UserEmail: "high@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 40, UserLimit: 200, UserUsage: 40}},
		},
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		acc := pool.Next()
		if acc == nil {
			t.Fatal("expected non-nil account")
		}
		seen[acc.UserEmail]++
	}
	for _, email := range []string{"low@example.com", "high@example.com"} {
		if seen[email] != 2 {
			t.Fatalf("expected each account called 2 times in 4 calls, got distribution: %v", seen)
		}
	}
}

func TestNextExcludingRoundRobinsAmongUntried(t *testing.T) {
	a := &Account{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 20, UserLimit: 200, UserUsage: 20}}
	b := &Account{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 80, UserLimit: 200, UserUsage: 80}}
	c := &Account{UserEmail: "c@example.com", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 200, SpaceUsage: 170, UserLimit: 200, UserUsage: 170}}
	pool := &AccountPool{
		accounts: []*Account{a, b, c},
	}

	// Exclude 'a': both b and c must appear, 'a' must never appear
	exclude := map[*Account]bool{a: true}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		acc := pool.NextExcluding(exclude)
		if acc == nil {
			t.Fatal("expected non-nil account")
		}
		if acc.UserEmail == "a@example.com" {
			t.Fatal("excluded account should not be returned")
		}
		seen[acc.UserEmail]++
	}
	if seen["b@example.com"] == 0 || seen["c@example.com"] == 0 {
		t.Fatalf("expected both b and c to appear, got: %v", seen)
	}
}

func TestBasicRemainingUsesMostConstrainedQuota(t *testing.T) {
	info := &QuotaInfo{
		SpaceLimit: 200,
		SpaceUsage: 20,
		UserLimit:  200,
		UserUsage:  190,
	}

	if got := basicRemaining(info); got != 10 {
		t.Fatalf("expected effective remaining 10, got %d", got)
	}
}

func TestIsFreePlanTreatsPersonalWithPremiumAsPaid(t *testing.T) {
	acc := &Account{
		PlanType: "personal",
		QuotaInfo: &QuotaInfo{
			HasPremium:     true,
			PremiumBalance: 1300000,
			PremiumLimit:   1300000,
		},
	}

	if isFreePlan(acc) {
		t.Fatal("expected personal account with premium credits to be treated as paid")
	}
}
