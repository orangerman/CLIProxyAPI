package redisqueue

import (
	"context"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageHistoryCapturesResolvedIPWithoutTrustingRawForwardedHeader(t *testing.T) {
	withEnabledQueue(t, func() {
		previous := UsageStatisticsEnabled()
		SetUsageStatisticsEnabled(true)
		t.Cleanup(func() { SetUsageStatisticsEnabled(previous) })
		usagehistory.Configure(t.TempDir())
		at := time.Now()
		for _, metadata := range []internallogging.ClientRequestMetadata{
			{ClientIP: "192.0.2.10", ResolvedClientIP: "203.0.113.5", XForwardedFor: "198.51.100.99"},
			{ClientIP: "192.0.2.10", XForwardedFor: "198.51.100.99"},
		} {
			ctx := internallogging.WithClientRequestMetadata(context.Background(), metadata)
			(&usageQueuePlugin{}).HandleUsage(ctx, coreusage.Record{RequestedAt: at, Model: "test", Provider: "codex"})
		}
		summary, err := usagehistory.Query(context.Background(), at.Add(-time.Hour), at.Add(time.Hour), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(summary.ClientIPs) != 2 || summary.ClientIPs[0] != "192.0.2.10" || summary.ClientIPs[1] != "203.0.113.5" {
			t.Fatalf("unexpected caller IPs: %v", summary.ClientIPs)
		}
	})
}
