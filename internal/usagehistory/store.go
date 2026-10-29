// Package usagehistory keeps a non-consuming, credential-free usage journal for the dashboard.
package usagehistory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Store writes one sanitized JSON line per execution, partitioned by UTC date.
// Completed records survive restarts; queue consumers never remove these records.
type Store struct {
	dir         string
	mu          sync.Mutex
	writeFailed atomic.Bool
}

var current atomic.Pointer[Store]

// Configure selects storage beside the server config, independently of the usage queue.
func Configure(dir string)  { current.Store(New(dir)) }
func New(dir string) *Store { return &Store{dir: dir} }

// Record is called by the existing asynchronous usage sink, with the same statistics toggle.
func Record(record coreusage.Record, clientIP string) error {
	store := current.Load()
	if store == nil {
		return nil
	}
	err := store.Append(record, clientIP)
	store.writeFailed.Store(err != nil)
	return err
}

type journalRecord struct {
	Timestamp    time.Time                `json:"timestamp"`
	Model        string                   `json:"model"`
	PricingModel string                   `json:"pricing_model,omitempty"`
	ServiceTier  string                   `json:"service_tier,omitempty"`
	Failed       bool                     `json:"failed"`
	Tokens       coreusage.TokenBreakdown `json:"tokens"`
	ClientIP     string                   `json:"client_ip,omitempty"`
}

// NormalizeClientIP uses one representation for IPv4, IPv6 and mapped IPv4 addresses.
func NormalizeClientIP(value string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

func (s *Store) Append(record coreusage.Record, clientIP string) error {
	if s == nil || s.dir == "" {
		return errors.New("usage history storage unavailable")
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	tier := record.ResponseServiceTier
	if tier == "" {
		tier = record.EffectiveServiceTier
	}
	if tier == "" {
		tier = record.ServiceTier
	}
	payload, err := json.Marshal(journalRecord{Timestamp: timestamp.UTC(), Model: model,
		PricingModel: strings.TrimSpace(record.ResponseModel), ServiceTier: tier,
		Failed: record.Failed, Tokens: detail.TokenBreakdown, ClientIP: NormalizeClientIP(clientIP)})
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.dir, timestamp.UTC().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, errWrite := file.Write(payload)
	errClose := file.Close()
	return errors.Join(errWrite, errClose)
}

// ModelUsage reports disjoint token categories; reasoning is already included in output.
type ModelUsage struct {
	Model              string  `json:"model"`
	Requests           int64   `json:"requests"`
	Failed             int64   `json:"failed"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheWriteTokens   int64   `json:"cache_write_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	UnclassifiedTokens int64   `json:"unclassified_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	EstimatedCostUSD   float64 `json:"estimated_cost_usd"`
	UnpricedRequests   int64   `json:"unpriced_requests"`
}

type Summary struct {
	From         time.Time    `json:"from"`
	To           time.Time    `json:"to"`
	Models       []ModelUsage `json:"models"`
	PricingDate  string       `json:"pricing_date"`
	HistoryStart string       `json:"history_start,omitempty"`
	ClientIPs    []string     `json:"client_ips"`
}

// Query uses an inclusive start and exclusive end, and never pops the usage queue.
func Query(ctx context.Context, from, to time.Time, clientIP string) (Summary, error) {
	store := current.Load()
	if store == nil {
		return Summary{}, errors.New("usage history storage unavailable")
	}
	return store.Query(ctx, from, to, clientIP)
}

func (s *Store) Query(ctx context.Context, from, to time.Time, clientIP string) (Summary, error) {
	result := Summary{From: from, To: to, Models: []ModelUsage{}, PricingDate: PricingDate, ClientIPs: []string{}}
	if clientIP != "" {
		clientIP = NormalizeClientIP(clientIP)
		if clientIP == "" {
			return result, errors.New("invalid client IP")
		}
	}
	if s.writeFailed.Load() {
		return result, errors.New("usage history write failed")
	}
	if errContext := ctx.Err(); errContext != nil {
		return result, errContext
	}
	if !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		return result, errors.New("usage range must be positive and at most 366 days")
	}
	files, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	byModel := make(map[string]*ModelUsage)
	clientIPs := make(map[string]bool)
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		day, errDay := time.Parse("2006-01-02", strings.TrimSuffix(entry.Name(), ".jsonl"))
		if errDay != nil {
			continue
		}
		if result.HistoryStart == "" {
			result.HistoryStart = day.Format("2006-01-02")
		}
		if !day.Before(to) || !day.AddDate(0, 0, 1).After(from) {
			continue
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if err = s.readDay(ctx, entry.Name(), from, to, clientIP, clientIPs, byModel); err != nil {
			return result, err
		}
	}
	for _, model := range byModel {
		result.Models = append(result.Models, *model)
	}
	for ip := range clientIPs {
		result.ClientIPs = append(result.ClientIPs, ip)
	}
	sort.Strings(result.ClientIPs)
	sort.Slice(result.Models, func(i, j int) bool {
		a, b := result.Models[i], result.Models[j]
		if a.TotalTokens != b.TotalTokens {
			return a.TotalTokens > b.TotalTokens
		}
		return a.Model < b.Model
	})
	return result, nil
}

func (s *Store) readDay(ctx context.Context, name string, from, to time.Time, clientIP string, clientIPs map[string]bool, byModel map[string]*ModelUsage) error {
	// Snapshot the complete file size under the append lock. Reading does not hold up the sink.
	s.mu.Lock()
	file, err := os.Open(filepath.Join(s.dir, name))
	var size int64
	if err == nil {
		info, errStat := file.Stat()
		err = errStat
		if err == nil {
			size = info.Size()
		}
	}
	s.mu.Unlock()
	if file != nil {
		defer func() { _ = file.Close() }()
	}
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(io.NewSectionReader(file, 0, size))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return err
		}
		var record journalRecord
		if err = json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("invalid usage history in %s", name)
		}
		if record.Timestamp.Before(from) || !record.Timestamp.Before(to) {
			continue
		}
		ip := NormalizeClientIP(record.ClientIP)
		if ip != "" {
			clientIPs[ip] = true
		}
		if clientIP != "" && ip != clientIP {
			continue
		}
		model := byModel[record.Model]
		if model == nil {
			model = &ModelUsage{Model: record.Model}
			byModel[record.Model] = model
		}
		model.Requests++
		if record.Failed {
			model.Failed++
		}
		tokens := record.Tokens
		model.InputTokens += tokens.Input.TotalTokens
		model.OutputTokens += tokens.Output.TotalTokens
		model.CacheReadTokens += tokens.Input.CacheReadTokens
		model.CacheWriteTokens += tokens.Input.CacheWriteTokens
		model.ReasoningTokens += tokens.Output.ReasoningTokens
		model.UnclassifiedTokens += tokens.UnclassifiedTokens
		model.TotalTokens += tokens.TotalTokens
		cost, known := estimateCost(record)
		if known {
			model.EstimatedCostUSD += cost
		} else {
			model.UnpricedRequests++
		}
	}
	return scanner.Err()
}
