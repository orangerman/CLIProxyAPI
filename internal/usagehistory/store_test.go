package usagehistory

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestHistoryPersistsAndQueriesWithoutConsuming(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	start := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)
	for _, timestamp := range []time.Time{start.Add(-time.Second), start, start.Add(24 * time.Hour)} {
		err := store.Append(coreusage.Record{
			RequestedAt: timestamp, Model: "gpt-6.1-sol", Provider: "codex",
			APIKey: "secret-client-key", AuthID: "secret-credential", Source: "private-email",
			Detail: coreusage.Detail{TokenBreakdown: coreusage.NewSubsetTokenBreakdown(100, 30, 0, 40, 10, 140)},
		}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		// Reopening reproduces a restart; repeated queries must return identical totals.
		summary, err := New(dir).Query(context.Background(), start, start.Add(24*time.Hour), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(summary.Models) != 1 {
			t.Fatalf("models = %v", summary.Models)
		}
		model := summary.Models[0]
		if model.Requests != 1 || model.TotalTokens != 140 || model.InputTokens != 100 || model.OutputTokens != 40 || model.CacheReadTokens != 30 || model.ReasoningTokens != 10 {
			t.Fatalf("incorrect boundaries or double-counted tokens: %+v", model)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		contents, errRead := os.ReadFile(file)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if strings.Contains(string(contents), "secret-") || strings.Contains(string(contents), "private-email") {
			t.Fatal("usage history retained credential material")
		}
	}
}

func TestHistoryRejectsInvalidRangesAndCancellation(t *testing.T) {
	store := New(t.TempDir())
	now := time.Now()
	for _, end := range []time.Time{now, now.Add(-time.Hour), now.Add(367 * 24 * time.Hour)} {
		if _, err := store.Query(context.Background(), now, end, ""); err == nil {
			t.Fatal("invalid range accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Append(coreusage.Record{Model: "test", RequestedAt: now}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Query(ctx, now.Add(-time.Hour), now.Add(time.Hour), ""); err == nil {
		t.Fatal("canceled query succeeded")
	}
}

func TestOfficialPricingDoesNotDoubleBillCachedOrReasoningTokens(t *testing.T) {
	record := journalRecord{Model: "gpt-6.1-sol", Tokens: coreusage.NewSubsetTokenBreakdown(100000, 80000, 0, 50000, 20000, 150000)}
	cost, known := estimateCost(record)
	if !known || math.Abs(cost-.548) > 1e-9 {
		t.Fatalf("cost=%v known=%v", cost, known)
	}
	record.Model = "gpt-6.1-sol-2026-10-01"
	if _, known = estimateCost(record); !known {
		t.Fatal("dated snapshot did not resolve")
	}
	record.Model = "gpt-6.1-sol-custom"
	if _, known = estimateCost(record); known {
		t.Fatal("invented model was silently priced")
	}
}

func TestOfficialPricingHandlesLongContextTiersAndUnknowns(t *testing.T) {
	record := journalRecord{Model: "gpt-6.1-sol", ServiceTier: "priority", Tokens: coreusage.NewSubsetTokenBreakdown(300000, 0, 0, 10000, 2000, 310000)}
	cost, known := estimateCost(record)
	if !known || math.Abs(cost-2.7) > 1e-9 {
		t.Fatalf("cost=%v known=%v", cost, known)
	}
	record.ServiceTier = "unknown-tier"
	if _, known = estimateCost(record); known {
		t.Fatal("unknown tier was priced")
	}
	record.ServiceTier = "default"
	record.Tokens = coreusage.TokenBreakdown{}
	if _, known = estimateCost(record); known {
		t.Fatal("unclassified usage was priced")
	}
}

func TestHistoryFiltersCanonicalCallerIPsAndRetainsLegacyTotals(t *testing.T) {
	store := New(t.TempDir())
	from := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	for _, ip := range []string{"192.0.2.10", "::ffff:192.0.2.10", "2001:0db8::1", ""} {
		if err := store.Append(coreusage.Record{
			RequestedAt: from, Model: "gpt-6.1-sol", Provider: "codex",
			Detail: coreusage.Detail{TokenBreakdown: coreusage.NewSubsetTokenBreakdown(100, 30, 0, 40, 10, 140)},
		}, ip); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		ip       string
		requests int64
	}{{"", 4}, {"192.0.2.10", 2}, {"::ffff:192.0.2.10", 2}, {"2001:db8::1", 1}, {"192.0.2.99", 0}} {
		summary, err := store.Query(context.Background(), from, to, test.ip)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(summary.ClientIPs, ",") != "192.0.2.10,2001:db8::1" {
			t.Fatalf("IP choices changed with selected filter: %v", summary.ClientIPs)
		}
		var requests, tokens int64
		for _, model := range summary.Models {
			requests += model.Requests
			tokens += model.TotalTokens
		}
		if requests != test.requests || tokens != test.requests*140 {
			t.Fatalf("IP %q: requests=%d tokens=%d", test.ip, requests, tokens)
		}
	}
	outside, err := store.Query(context.Background(), to, to.AddDate(0, 0, 1), "")
	if err != nil || len(outside.ClientIPs) != 0 {
		t.Fatalf("outside period IPs: %+v %v", outside, err)
	}
	if _, err = store.Query(context.Background(), from, to, "not-an-ip"); err == nil {
		t.Fatal("invalid IP filter accepted")
	}
}
