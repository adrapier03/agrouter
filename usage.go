package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// TokenUsage holds counts for a single completed chat request.
type TokenUsage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	Cached int `json:"cached_tokens"`
	Total  int `json:"total_tokens"`
}

// UsageRecord is a single persistent record written to usage.jsonl.
type UsageRecord struct {
	Timestamp int64  `json:"ts"`    // Unix epoch seconds
	Model     string `json:"model"` // e.g. "gemini-3.8-flash-high"
	Account   string `json:"acc"`   // Google account email
	Key       string `json:"key"`   // API Key name or "default"
	Input     int    `json:"inp"`   // Prompt tokens
	Output    int    `json:"out"`   // Completion tokens
	Cached    int    `json:"cache"` // Cached prompt tokens
	Total     int    `json:"tot"`   // Total tokens
}

// PeriodSummary summarizes metrics over a specific timeframe (today, 1hari, etc.).
type PeriodSummary struct {
	Requests     int64                    `json:"requests"`
	InputTokens  int64                    `json:"inputTokens"`
	OutputTokens int64                    `json:"outputTokens"`
	CachedTokens int64                    `json:"cachedTokens"`
	TotalTokens  int64                    `json:"totalTokens"`
	ByModel      map[string]*ModelSummary `json:"byModel"`
	ByAccount    map[string]*ModelSummary `json:"byAccount"`
	ByKey        map[string]*ModelSummary `json:"byKey"`
}

type ModelSummary struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	CachedTokens int64 `json:"cachedTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type DaySummary struct {
	Date         string `json:"date"` // YYYY-MM-DD
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	CachedTokens int64  `json:"cachedTokens"`
	TotalTokens  int64  `json:"totalTokens"`
}

// UsageReport is the complete payload returned to the dashboard.
type UsageReport struct {
	GeneratedAt string                    `json:"generatedAt"`
	Timeframes  map[string]*PeriodSummary `json:"timeframes"`
	Daily       []*DaySummary             `json:"daily"`
}

type UsageTracker struct {
	mu       sync.RWMutex
	filePath string
	records  []UsageRecord
}

var usageTracker *UsageTracker

func initUsageTracker(filePath, logPath string) (*UsageTracker, error) {
	ut := &UsageTracker{
		filePath: filePath,
		records:  make([]UsageRecord, 0, 2048),
	}

	// Try reading existing usage.jsonl
	f, err := os.Open(filePath)
	if err == nil {
		defer f.Close()
		cutoff := time.Now().Add(-95 * 24 * time.Hour).Unix()
		sc := bufio.NewScanner(f)
		buf := make([]byte, 64*1024)
		sc.Buffer(buf, 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var rec UsageRecord
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				if rec.Timestamp >= cutoff {
					ut.records = append(ut.records, rec)
				}
			}
		}
	}

	// If no records loaded yet, seed from historical agrouter.log if available
	if len(ut.records) == 0 && logPath != "" {
		ut.seedFromLog(logPath)
	}

	return ut, nil
}

// seedFromLog reads historical "chat OK" lines from agrouter.log and populates usage.jsonl
func (ut *UsageTracker) seedFromLog(logPath string) {
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer f.Close()

	// Example: 2026/09/14 01:47:56 chat OK 127.0.0.1:43014 model=gemini-3.8-flash-high acc=tabewulugi@gamaa.id attempt=1
	pat := regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) chat OK [^ ]+ model=([^ ]+) acc=([^ ]+)`)

	sc := bufio.NewScanner(f)
	var seeded []UsageRecord
	cutoff := time.Now().Add(-95 * 24 * time.Hour).Unix()

	for sc.Scan() {
		line := sc.Text()
		m := pat.FindStringSubmatch(line)
		if len(m) == 4 {
			t, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.UTC)
			if err != nil {
				continue
			}
			ts := t.Unix()
			if ts < cutoff {
				continue
			}
			// Reasonable historical baseline: ~1,500 input tokens, ~150 output tokens
			inp := 1500
			out := 150
			seeded = append(seeded, UsageRecord{
				Timestamp: ts,
				Model:     m[2],
				Account:   m[3],
				Key:       "default",
				Input:     inp,
				Output:    out,
				Cached:    0,
				Total:     inp + out,
			})
		}
	}

	if len(seeded) > 0 {
		ut.records = seeded
		_ = ut.persistAll()
	}
}

func (ut *UsageTracker) persistAll() error {
	tmp := ut.filePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	for _, r := range ut.records {
		b, err := json.Marshal(r)
		if err == nil {
			bw.Write(b)
			bw.WriteByte('\n')
		}
	}
	bw.Flush()
	f.Close()
	return os.Rename(tmp, ut.filePath)
}

// Record appends a completed chat request to memory and disk.
func (ut *UsageTracker) Record(model, acc, key string, u *TokenUsage) {
	if ut == nil || u == nil {
		return
	}
	if key == "" {
		key = "default"
	}
	rec := UsageRecord{
		Timestamp: time.Now().Unix(),
		Model:     model,
		Account:   acc,
		Key:       key,
		Input:     u.Input,
		Output:    u.Output,
		Cached:    u.Cached,
		Total:     u.Total,
	}
	if rec.Total == 0 {
		rec.Total = rec.Input + rec.Output
	}

	ut.mu.Lock()
	ut.records = append(ut.records, rec)
	// Prune memory if it exceeds 100k
	if len(ut.records) > 100000 {
		cutoff := time.Now().Add(-90 * 24 * time.Hour).Unix()
		trimmed := make([]UsageRecord, 0, len(ut.records)/2)
		for _, r := range ut.records {
			if r.Timestamp >= cutoff {
				trimmed = append(trimmed, r)
			}
		}
		ut.records = trimmed
	}
	ut.mu.Unlock()

	// Append to file asynchronously / safely
	go func(r UsageRecord) {
		b, err := json.Marshal(r)
		if err != nil {
			return
		}
		b = append(b, '\n')
		f, err := os.OpenFile(ut.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(b)
	}(rec)
}

// GetReport computes aggregated metrics for today, 1hari, 7hari, 30hari, 60hari.
func (ut *UsageTracker) GetReport(tzOffsetMin int) *UsageReport {
	// Determine user location based on tzOffsetMin (e.g. -420 for UTC+7)
	// Fallback to WIB (UTC+7) if not provided.
	var loc *time.Location
	if tzOffsetMin != 0 {
		loc = time.FixedZone("client", -tzOffsetMin*60)
	} else {
		loc = time.FixedZone("WIB", 7*3600)
	}

	now := time.Now()
	nowInLoc := now.In(loc)

	// Thresholds:
	// 1. today: midnight in local timezone
	y, m, d := nowInLoc.Date()
	todayStart := time.Date(y, m, d, 0, 0, 0, 0, loc).Unix()

	// 2. 1hari: last 24h
	oneDayAgo := now.Add(-24 * time.Hour).Unix()

	// 3. 7hari: last 7 days
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour).Unix()

	// 4. 30hari: last 30 days
	thirtyDaysAgo := now.Add(-30 * 24 * time.Hour).Unix()

	// 5. 60hari: last 60 days
	sixtyDaysAgo := now.Add(-60 * 24 * time.Hour).Unix()

	ut.mu.RLock()
	defer ut.mu.RUnlock()

	periods := map[string]*PeriodSummary{
		"today":  newPeriodSummary(),
		"1hari":  newPeriodSummary(),
		"7hari":  newPeriodSummary(),
		"30hari": newPeriodSummary(),
		"60hari": newPeriodSummary(),
	}

	dailyMap := make(map[string]*DaySummary)

	addRecordToPeriod := func(ps *PeriodSummary, r *UsageRecord) {
		ps.Requests++
		ps.InputTokens += int64(r.Input)
		ps.OutputTokens += int64(r.Output)
		ps.CachedTokens += int64(r.Cached)
		ps.TotalTokens += int64(r.Total)

		// By Model
		mName := r.Model
		if mName == "" {
			mName = "unknown"
		}
		ms, ok := ps.ByModel[mName]
		if !ok {
			ms = &ModelSummary{}
			ps.ByModel[mName] = ms
		}
		ms.Requests++
		ms.InputTokens += int64(r.Input)
		ms.OutputTokens += int64(r.Output)
		ms.CachedTokens += int64(r.Cached)
		ms.TotalTokens += int64(r.Total)

		// By Account
		accName := r.Account
		if accName == "" {
			accName = "unknown"
		}
		as, ok := ps.ByAccount[accName]
		if !ok {
			as = &ModelSummary{}
			ps.ByAccount[accName] = as
		}
		as.Requests++
		as.InputTokens += int64(r.Input)
		as.OutputTokens += int64(r.Output)
		as.CachedTokens += int64(r.Cached)
		as.TotalTokens += int64(r.Total)

		// By Key
		kName := r.Key
		if kName == "" {
			kName = "default"
		}
		ks, ok := ps.ByKey[kName]
		if !ok {
			ks = &ModelSummary{}
			ps.ByKey[kName] = ks
		}
		ks.Requests++
		ks.InputTokens += int64(r.Input)
		ks.OutputTokens += int64(r.Output)
		ks.CachedTokens += int64(r.Cached)
		ks.TotalTokens += int64(r.Total)
	}

	for _, r := range ut.records {
		ts := r.Timestamp
		if ts >= sixtyDaysAgo {
			addRecordToPeriod(periods["60hari"], &r)
		}
		if ts >= thirtyDaysAgo {
			addRecordToPeriod(periods["30hari"], &r)
		}
		if ts >= sevenDaysAgo {
			addRecordToPeriod(periods["7hari"], &r)
		}
		if ts >= oneDayAgo {
			addRecordToPeriod(periods["1hari"], &r)
		}
		if ts >= todayStart {
			addRecordToPeriod(periods["today"], &r)
		}

		// Daily tracking (keep last 60 days)
		if ts >= sixtyDaysAgo {
			dateKey := time.Unix(ts, 0).In(loc).Format("2006-01-02")
			ds, ok := dailyMap[dateKey]
			if !ok {
				ds = &DaySummary{Date: dateKey}
				dailyMap[dateKey] = ds
			}
			ds.Requests++
			ds.InputTokens += int64(r.Input)
			ds.OutputTokens += int64(r.Output)
			ds.CachedTokens += int64(r.Cached)
			ds.TotalTokens += int64(r.Total)
		}
	}

	// Sort daily descending
	dailyList := make([]*DaySummary, 0, len(dailyMap))
	for _, ds := range dailyMap {
		dailyList = append(dailyList, ds)
	}
	sort.Slice(dailyList, func(i, j int) bool {
		return dailyList[i].Date > dailyList[j].Date
	})

	return &UsageReport{
		GeneratedAt: nowInLoc.Format(time.RFC3339),
		Timeframes:  periods,
		Daily:       dailyList,
	}
}

func newPeriodSummary() *PeriodSummary {
	return &PeriodSummary{
		ByModel:   make(map[string]*ModelSummary),
		ByAccount: make(map[string]*ModelSummary),
		ByKey:     make(map[string]*ModelSummary),
	}
}

// handleUsage serves GET /admin/usage
func handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"GET only"}`, http.StatusMethodNotAllowed)
		return
	}

	tzOffset := 0
	if tzStr := r.URL.Query().Get("tz"); tzStr != "" {
		fmt.Sscanf(tzStr, "%d", &tzOffset)
	}

	rep := usageTracker.GetReport(tzOffset)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rep)
}
