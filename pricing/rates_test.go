package pricing

import "testing"

func TestMsatPerThousandTokens(t *testing.T) {
	// DeepSeek V4 Flash input, $0.13/M, 10% margin, BTC $60,000:
	// 0.13 * 1e8 * 1.1 / 60000 = 238.33 -> 239 msat per thousand tokens.
	if got := MsatPerThousandTokens(0.13, 10, 60000); got != 239 {
		t.Fatalf("got %d, want 239", got)
	}
	if got := MsatPerThousandTokens(0, 10, 60000); got != 0 {
		t.Fatalf("zero price must be 0, got %d", got)
	}
	if got := MsatPerThousandTokens(1e-9, 0, 60000); got != 1 {
		t.Fatalf("tiny price must round up to 1, got %d", got)
	}
}

func TestCachedPermille(t *testing.T) {
	cases := []struct {
		in, cached float64
		want       int64
	}{
		{3, 0.3, 100}, {0.13, 0.028, 216}, {1, 0, 0}, {0, 0.1, 0}, {1, 2, 1000},
	}
	for _, c := range cases {
		got := cachedPermille(&ModelUSD{InputUSDPerMTok: c.in, CachedInputUSDPerMTok: c.cached})
		if got != c.want {
			t.Errorf("in %v cached %v: got %d want %d", c.in, c.cached, got, c.want)
		}
	}
}

// meterd quotes a bundle at ceil(tokens * (in + out) / 2,000,000) sats.
func quote(tokens, in, out int64) int64 {
	v := tokens * (in + out)
	return (v + 2_000_000 - 1) / 2_000_000
}

func TestBundleTokensForSats(t *testing.T) {
	for _, r := range [][2]int64{{239, 477}, {1, 1}, {5000, 25000}, {999_999, 999_999}} {
		for _, sats := range []int64{1, 1000, 2750, 100_000} {
			tokens := bundleTokensForSats(sats, r[0], r[1])
			if got := quote(tokens, r[0], r[1]); got != sats {
				t.Errorf("rates %v, %d sats: %d tokens quote %d", r, sats, tokens, got)
			}
			if quote(tokens+1, r[0], r[1]) <= sats {
				t.Errorf("rates %v, %d sats: bundle not maximal", r, sats)
			}
		}
	}
	if bundleTokensForSats(0, 1, 1) != 0 || bundleTokensForSats(10, 0, 0) != 0 {
		t.Fatal("degenerate inputs must return 0")
	}
}
