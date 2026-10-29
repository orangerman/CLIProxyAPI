package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
)

// GetUsageSummary queries saved executions without consuming the upstream usage queue.
func (h *Handler) GetUsageSummary(c *gin.Context) {
	from, errFrom := time.Parse(time.RFC3339, c.Query("from"))
	to, errTo := time.Parse(time.RFC3339, c.Query("to"))
	if errFrom != nil || errTo != nil || !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from and to must be RFC3339 timestamps defining a positive range of at most 366 days"})
		return
	}
	clientIP := strings.TrimSpace(c.Query("client_ip"))
	if clientIP != "" {
		clientIP = usagehistory.NormalizeClientIP(clientIP)
		if clientIP == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "client_ip must be an IPv4 or IPv6 address"})
			return
		}
	}
	summary, err := usagehistory.Query(c.Request.Context(), from, to, clientIP)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage history unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"from": summary.From, "to": summary.To, "models": summary.Models,
		"pricing_date": summary.PricingDate, "history_start": summary.HistoryStart,
		"client_ip": clientIP, "client_ips": summary.ClientIPs,
		"recording_enabled": redisqueue.Enabled() && redisqueue.UsageStatisticsEnabled(),
	})
}
