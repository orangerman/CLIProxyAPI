package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGetUsageSummaryPreservesQueueAndCalendarBounds(t *testing.T) {
	withManagementUsageQueue(t, func() {
		previous := redisqueue.UsageStatisticsEnabled()
		redisqueue.SetUsageStatisticsEnabled(true)
		t.Cleanup(func() { redisqueue.SetUsageStatisticsEnabled(previous) })
		usagehistory.Configure(t.TempDir())
		from := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
		to := from.AddDate(0, 0, 1)
		for _, at := range []time.Time{from.Add(-time.Second), from, to} {
			if err := usagehistory.Record(coreusage.Record{
				RequestedAt: at, Model: "gpt-6.1-sol", Provider: "codex",
				Detail: coreusage.Detail{TokenBreakdown: coreusage.NewSubsetTokenBreakdown(100, 30, 0, 40, 10, 140)},
			}, "192.0.2.10"); err != nil {
				t.Fatal(err)
			}
		}
		redisqueue.Enqueue([]byte(`{"id":1}`))
		query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
		for _, clientIP := range []string{"", "192.0.2.10", "::ffff:192.0.2.10"} {
			query.Set("client_ip", clientIP)
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/summary?"+query.Encode(), nil)
			(&Handler{}).GetUsageSummary(ctx)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var payload struct {
				Models           []usagehistory.ModelUsage `json:"models"`
				RecordingEnabled bool                      `json:"recording_enabled"`
				ClientIPs        []string                  `json:"client_ips"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if !payload.RecordingEnabled || len(payload.Models) != 1 || payload.Models[0].Requests != 1 || payload.Models[0].TotalTokens != 140 {
				t.Fatalf("unexpected summary: %+v", payload)
			}
			if len(payload.ClientIPs) != 1 || payload.ClientIPs[0] != "192.0.2.10" {
				t.Fatalf("unexpected IP choices: %v", payload.ClientIPs)
			}
		}
		remaining := redisqueue.PopOldest(10)
		if len(remaining) != 1 || string(remaining[0]) != `{"id":1}` {
			t.Fatalf("summary consumed usage queue: %q", remaining)
		}
	})
}

func TestGetUsageSummaryRejectsInvalidRange(t *testing.T) {
	for _, query := range []string{
		"", "from=bad&to=bad",
		"from=2026-10-29T00:00:00Z&to=2026-10-28T00:00:00Z",
		"from=2025-01-01T00:00:00Z&to=2026-10-29T00:00:00Z",
		"from=2026-10-28T00:00:00Z&to=2026-10-29T00:00:00Z&client_ip=invalid",
	} {
		t.Run(query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/summary?"+query, nil)
			(&Handler{}).GetUsageSummary(ctx)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
