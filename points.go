package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pointsBalance is GET /api/v1/quota/points/total.
type pointsBalance struct {
	TotalPoints       flexFloat `json:"total_points"`
	OrganizationTotal flexFloat `json:"organization_total_points"`
	Expiring          []struct {
		QuotaID         flexID    `json:"quota_id"`
		ExpiresAt       string    `json:"expires_at"`
		RemainingPoints flexFloat `json:"remaining_points"`
	} `json:"expiring_quota_details"`
	NextCycleGrant json.RawMessage `json:"next_cycle_grant"`
}

// pointsRecord is one row of POST /api/v1/quota/points/records/queries.
// amount is a quoted string upstream ("-68", "200").
type pointsRecord struct {
	ID           flexID     `json:"id"`
	Amount       flexFloat  `json:"amount"`
	UserID       flexString `json:"user_id"`
	TopicID      flexString `json:"topic_id"`
	Organization flexString `json:"organization_code"`
	Label        string     `json:"label"`
	Description  string     `json:"description"`
	Remark       string     `json:"remark"`
	CreatedAt    string     `json:"created_at"`
	UpdatedAt    string     `json:"updated_at"`
	BusinessType string     `json:"business_type"`
}

type pointsCache struct {
	mu      sync.Mutex
	at      time.Time
	ok      bool
	balance pointsBalance
	err     string
	recAt   time.Time
	recErr  string
}

const (
	pointsCacheTTL     = 60 * time.Second
	pointsFailCacheTTL = 10 * time.Second
)

func (a *app) pointsFor(accountID string) *pointsCache {
	a.pointsMu.Lock()
	defer a.pointsMu.Unlock()
	cache, ok := a.points[accountID]
	if !ok {
		cache = &pointsCache{}
		a.points[accountID] = cache
	}
	return cache
}

// balance returns the cached balance, refreshing when stale. A failed refresh
// keeps the previous good numbers instead of blanking the console.
func (a *app) balance(ctx context.Context, account *credential, force bool) (pointsBalance, error) {
	cache := a.pointsFor(account.ID)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	ttl := pointsCacheTTL
	if !cache.ok {
		ttl = pointsFailCacheTTL
	}
	if !force && !cache.at.IsZero() && time.Since(cache.at) < ttl {
		return cache.balance, nil
	}
	data, err := a.upstream().callAsset(ctx, http.MethodGet, "/api/v1/quota/points/total", account.Authorization, account.OrganizationCode, nil)
	if err != nil {
		cache.at = time.Now()
		cache.err = asFailure(err).Message
		if cache.ok {
			return cache.balance, nil
		}
		return pointsBalance{}, err
	}
	var balance pointsBalance
	if err := json.Unmarshal(data, &balance); err != nil {
		cache.at = time.Now()
		cache.err = "解析积分余额失败：" + err.Error()
		if cache.ok {
			return cache.balance, nil
		}
		return pointsBalance{}, fmt.Errorf("%s", cache.err)
	}
	cache.balance = balance
	cache.ok = true
	cache.err = ""
	cache.at = time.Now()
	a.noteExpiry(account.ID, balance, cache.at)
	return balance, nil
}

// expiryScore says how close an account is to losing points and how many of them
// are at risk. Accounts without deadline-carrying quotas score zero, so routing
// falls back to the plain priority/rotation order.
type expiryScore struct {
	Soon     float64   `json:"soon"`
	Earliest time.Time `json:"earliest"`
	At       time.Time `json:"at"`
}

// expiringSoonHorizon is the window the console calls "即将过期".
const expiringSoonHorizon = 30 * 24 * time.Hour

// expiryScoreOf reduces a balance to the numbers the router needs. Pure, so the
// ranking stays testable.
func expiryScoreOf(balance pointsBalance, now time.Time) expiryScore {
	limit := now.Add(expiringSoonHorizon)
	var score expiryScore
	score.At = now
	for _, quota := range balance.Expiring {
		points := quota.RemainingPoints.Float64()
		if points <= 0 {
			continue
		}
		when, ok := parseServerTime(quota.ExpiresAt)
		if !ok || when.After(limit) {
			continue
		}
		score.Soon += points
		if score.Earliest.IsZero() || when.Before(score.Earliest) {
			score.Earliest = when
		}
	}
	return score
}

// compareExpiry ranks two accounts: >0 means a should be spent before b.
func compareExpiry(a, b expiryScore) int {
	if a.Soon != b.Soon {
		if a.Soon <= 0 {
			return -1
		}
		if b.Soon <= 0 {
			return 1
		}
	}
	switch {
	case a.Earliest.IsZero() && !b.Earliest.IsZero():
		return -1
	case !a.Earliest.IsZero() && b.Earliest.IsZero():
		return 1
	case !a.Earliest.IsZero() && a.Earliest.Before(b.Earliest):
		return 1
	case !a.Earliest.IsZero() && a.Earliest.After(b.Earliest):
		return -1
	case a.Soon > b.Soon:
		return 1
	case a.Soon < b.Soon:
		return -1
	}
	return 0
}

func (a *app) noteExpiry(accountID string, balance pointsBalance, now time.Time) {
	score := expiryScoreOf(balance, now)
	a.expiryMu.Lock()
	defer a.expiryMu.Unlock()
	a.expiry[accountID] = score
}

func (a *app) expiryOf(accountID string) expiryScore {
	if !a.currentConfig().PreferExpiringPoints {
		return expiryScore{}
	}
	a.expiryMu.RLock()
	defer a.expiryMu.RUnlock()
	return a.expiry[accountID]
}

func (a *app) forgetExpiry(accountID string) {
	a.expiryMu.Lock()
	defer a.expiryMu.Unlock()
	delete(a.expiry, accountID)
}

// expiryLoop keeps the expiry scores warm so the router can rank accounts
// without waiting on the asset API inside a request.
func (a *app) expiryLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-ticker.C:
		}
		if !a.currentConfig().PreferExpiringPoints {
			continue
		}
		a.refreshExpiryScores(ctx)
	}
}

func (a *app) refreshExpiryScores(ctx context.Context) {
	for _, account := range a.pool.snapshot() {
		if ctx.Err() != nil {
			return
		}
		if !usable(account) || account.cooling() {
			continue
		}
		// balance() caches for a minute and keeps the last good numbers, so a
		// failing account neither blocks nor blanks the ranking.
		if _, err := a.balance(ctx, account, false); err != nil {
			continue
		}
	}
}

// records lists the point ledger.
func (a *app) records(ctx context.Context, account *credential, filterType int, filterRange string, pageSize int) ([]pointsRecord, error) {
	list, _, err := a.recordsPage(ctx, account, "", filterType, 1, pageSize)
	return list, err
}

// recordsPage is the raw ledger query: date "" means every day, filter_type 1 is
// everything, 2 income only, 3 expenditure only.
func (a *app) recordsPage(ctx context.Context, account *credential, date string, filterType int, page, pageSize int) ([]pointsRecord, int, error) {
	if filterType == 0 {
		filterType = 3 // Expenditure
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if page <= 0 {
		page = 1
	}
	body, _ := json.Marshal(map[string]any{
		"date":         date,
		"filter_type":  filterType,
		"filter_range": "personal",
		"page":         page,
		"page_size":    pageSize,
	})
	data, err := a.upstream().callAsset(ctx, http.MethodPost, "/api/v1/quota/points/records/queries", account.Authorization, account.OrganizationCode, body)
	if err != nil {
		return nil, 0, err
	}
	var payload struct {
		Total flexInt        `json:"total"`
		List  []pointsRecord `json:"list"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, 0, fmt.Errorf("解析积分流水失败：%w", err)
	}
	return payload.List, int(payload.Total.Int64()), nil
}

// recordsForDay walks every page of one day for one filter type. A day of model
// calls easily runs into the hundreds of rows, so the console cannot judge
// "already claimed today" from page one alone.
func (a *app) recordsForDay(ctx context.Context, account *credential, day string, filterType, maxPages int) ([]pointsRecord, error) {
	const pageSize = 100
	var out []pointsRecord
	for page := 1; page <= maxPages; page++ {
		list, total, err := a.recordsPage(ctx, account, day, filterType, page, pageSize)
		if err != nil {
			return out, err
		}
		out = append(out, list...)
		if len(list) < pageSize || page*pageSize >= total {
			break
		}
	}
	return out, nil
}

// todayTotals sums today's ledger for the console. Income and expenditure are
// queried separately: with filter_type 1 a 13-row income day hides under
// hundreds of consumption rows, which made the console claim "未领取" for an
// already claimed daily gift.
func (a *app) todayTotals(ctx context.Context, account *credential, day string) (gift, task, consumed float64, claimedAt string, err error) {
	income, errIncome := a.recordsForDay(ctx, account, day, 2, 3)
	if errIncome != nil {
		return 0, 0, 0, "", errIncome
	}
	expense, errExpense := a.recordsForDay(ctx, account, day, 3, 5)
	gift, task, consumed, claimedAt = sumTodayRecords(income, expense)
	if errExpense != nil {
		return gift, task, 0, claimedAt, errExpense
	}
	return gift, task, consumed, claimedAt, nil
}

// claimLoginGift calls the real daily claim endpoint. granted=false with the
// "already claimed" message is a normal, non-error outcome.
type loginGiftResult struct {
	Granted     bool      `json:"granted"`
	PointAmount flexFloat `json:"point_amount"`
	ExpireType  flexInt   `json:"expire_type"`
	Message     string    `json:"message"`
}

func (a *app) claimLoginGift(ctx context.Context, account *credential) (loginGiftResult, error) {
	var out loginGiftResult
	data, err := a.upstream().callAsset(ctx, http.MethodPost, "/api/v1/login-gift", account.Authorization, account.OrganizationCode, nil)
	if err != nil {
		return out, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return out, fmt.Errorf("解析登录积分响应失败：%w", err)
		}
	}
	return out, nil
}

type onboardingTask struct {
	Code         string    `json:"code"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Points       flexFloat `json:"points"`
	Status       string    `json:"status"`
	RewardStatus string    `json:"reward_status"`
	CompletedAt  *string   `json:"completed_at"`
	GrantedAt    *string   `json:"reward_granted_at"`
	ExpiresAt    *string   `json:"expires_at"`
}

func (a *app) onboardingTasks(ctx context.Context, account *credential) ([]onboardingTask, string, error) {
	data, err := a.upstream().callAsset(ctx, http.MethodGet, "/api/v1/onboarding/tasks", account.Authorization, account.OrganizationCode, nil)
	if err != nil {
		return nil, "", err
	}
	var payload struct {
		ParticipantType string           `json:"participant_type"`
		Tasks           []onboardingTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, "", fmt.Errorf("解析新人任务失败：%w", err)
	}
	return payload.Tasks, payload.ParticipantType, nil
}

// completeOnboardingTask claims one manual reward. Codes 9303/9304 mean the
// task is not claimable and must not be retried.
func (a *app) completeOnboardingTask(ctx context.Context, account *credential, code string) (float64, error) {
	data, err := a.upstream().callAsset(ctx, http.MethodPut, "/api/v1/onboarding/tasks/"+code+"/completion", account.Authorization, account.OrganizationCode, nil)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Points flexFloat `json:"points"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &payload)
	}
	return payload.Points.Float64(), nil
}

// pointsJob is one finished request waiting for its ledger row to appear.
type pointsJob struct {
	seq      int64
	account  *credential
	model    string
	started  time.Time
	finished time.Time
}

// enqueuePoints hands the request to the single backfill worker. A full queue
// drops the entry instead of growing goroutines without bound.
func (a *app) enqueuePoints(job pointsJob) {
	a.mu.Lock()
	queue := a.pointsQueue
	a.mu.Unlock()
	if queue == nil {
		return
	}
	select {
	case queue <- job:
	default:
		a.log.warn("points", "积分回填队列已满，丢弃本次回填")
	}
}

// pointsWorker is the only goroutine that touches the ledger for backfill.
func (a *app) pointsWorker(ctx context.Context) {
	a.mu.Lock()
	queue := a.pointsQueue
	a.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-queue:
			a.attachRequestPoints(ctx, job)
		}
	}
}

// attachRequestPoints matches a finished request against the ledger and writes
// the consumed points back into the request log.
func (a *app) attachRequestPoints(parent context.Context, job pointsJob) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	// The ledger row is written asynchronously; give it a moment and retry.
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		records, err := a.records(ctx, job.account, 3, "personal", 30)
		if err != nil {
			continue
		}
		total, matched := a.claimModelRecords(job.account.ID, records, job.model, job.started, job.finished)
		if matched {
			if total != 0 {
				a.requests.setPoints(job.seq, total)
				a.metrics.points.Add(total)
			}
			return
		}
	}
}

// claimedRecords guards against attributing one ledger row to two requests.
var claimedRecords = struct {
	mu  sync.Mutex
	ids map[string]bool
}{ids: make(map[string]bool)}

// claimModelRecords sums the unclaimed magic_model_consume rows for one model
// inside the request's time window.
func (a *app) claimModelRecords(accountID string, records []pointsRecord, model string, started, finished time.Time) (int64, bool) {
	lower := started.Add(-5 * time.Second)
	upper := finished.Add(90 * time.Second)
	needle := "模型调用「" + model + "」"
	var total int64
	matched := false
	claimedRecords.mu.Lock()
	defer claimedRecords.mu.Unlock()
	for _, record := range records {
		if record.BusinessType != "magic_model_consume" {
			continue
		}
		if !strings.Contains(record.Description, needle) {
			continue
		}
		created, ok := parseServerTime(record.CreatedAt)
		if !ok || created.Before(lower) || created.After(upper) {
			continue
		}
		key := accountID + ":" + record.ID.String()
		if claimedRecords.ids[key] {
			continue
		}
		claimedRecords.ids[key] = true
		total += int64(record.Amount.Float64())
		matched = true
	}
	if len(claimedRecords.ids) > 20000 {
		claimedRecords.ids = make(map[string]bool)
	}
	return total, matched
}

// dailyPointsLoop claims the daily login gift and the manual onboarding rewards
// for every account, once per service day, with a per-account lock so two ticks
// never race on the same account.
func (a *app) dailyPointsLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	dayMu := sync.Mutex{}
	doneDay := make(map[string]string)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			cfg := a.currentConfig()
			if cfg.DailyPointsEnabled && claimWindowOpen(cfg.PointsScheduleTime, time.Now()) {
				dayMu.Lock()
				for _, account := range a.pool.snapshot() {
					if !usable(account) {
						continue
					}
					today := time.Now().In(serverZone).Format("2006-01-02")
					if doneDay[account.ID] == today {
						continue
					}
					if a.claimDaily(ctx, account, today) {
						doneDay[account.ID] = today
					}
				}
				dayMu.Unlock()
			}
			timer.Reset(30 * time.Minute)
		}
	}
}

// claimDaily returns true when everything claimable today has been claimed, so
// the caller can stop retrying until tomorrow.
func (a *app) claimDaily(ctx context.Context, account *credential, today string) bool {
	// One claim per account at a time; the lock is separate from the account
	// mutex so console reads never wait on a network call.
	account.claimMu.Lock()
	defer account.claimMu.Unlock()
	claimCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	gift, err := a.claimLoginGift(claimCtx, account)
	if err != nil {
		a.log.warn("points", fmt.Sprintf("账号 %s 领取每日登录积分失败：%s", account.label(), asFailure(err).Message))
		return false
	}
	if gift.Granted {
		a.log.info("points", fmt.Sprintf("账号 %s 到账每日登录积分 %s", account.label(), formatPoints(gift.PointAmount.Float64())))
		a.invalidatePoints(account.ID)
	}

	tasks, _, err := a.onboardingTasks(claimCtx, account)
	if err != nil {
		a.log.warn("points", fmt.Sprintf("账号 %s 读取新人任务失败：%s", account.label(), asFailure(err).Message))
		return gift.Message != "" || gift.Granted
	}
	pending := 0
	for _, task := range tasks {
		if task.Status != "PENDING" || task.RewardStatus != "PENDING" {
			continue
		}
		if task.Code == "first_ai_work" {
			continue // auto-completed by the client, not claimable
		}
		pending++
		points, errClaim := a.completeOnboardingTask(claimCtx, account, task.Code)
		if errClaim != nil {
			f := asFailure(errClaim)
			if strings.Contains(f.Message, "9303") || strings.Contains(f.Message, "9304") || strings.Contains(f.Message, "9302") {
				continue // terminal: not claimable / expired
			}
			a.log.warn("points", fmt.Sprintf("账号 %s 领取任务 %s 失败：%s", account.label(), task.Code, f.Message))
			return false
		}
		a.log.info("points", fmt.Sprintf("账号 %s 领取任务 %s 到账 %s 积分", account.label(), task.Code, formatPoints(points)))
		a.invalidatePoints(account.ID)
	}
	return pending == 0 || gift.Granted
}

func (a *app) invalidatePoints(accountID string) {
	a.pointsMu.Lock()
	cache, ok := a.points[accountID]
	a.pointsMu.Unlock()
	if !ok {
		return
	}
	cache.mu.Lock()
	cache.at = time.Time{}
	cache.recAt = time.Time{}
	cache.mu.Unlock()
	a.forgetExpiry(accountID)
}

func formatPoints(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%.2f", value)
}

// pointsSummary is what the console renders on the points tab.
type pointsSummary struct {
	Balance      pointsBalance    `json:"balance"`
	BalanceError string           `json:"balance_error,omitempty"`
	Gift         *loginGiftResult `json:"gift,omitempty"`
	Tasks        []onboardingTask `json:"tasks,omitempty"`
	Records      []pointsRecord   `json:"records,omitempty"`
	RecordsError string           `json:"records_error,omitempty"`
	Today        struct {
		GiftPoints      float64 `json:"gift_points"`
		TaskPoints      float64 `json:"task_points"`
		ConsumedPoints  float64 `json:"consumed_points"`
		LastClaimedDate string  `json:"last_claimed_date"`
	} `json:"today"`
}

// summarizePoints builds the console payload for one account.
func (a *app) summarizePoints(ctx context.Context, account *credential, includeRecords bool) pointsSummary {
	var out pointsSummary
	balance, err := a.balance(ctx, account, false)
	if err != nil {
		out.BalanceError = asFailure(err).Message
	} else {
		out.Balance = balance
	}
	tasks, _, errTasks := a.onboardingTasks(ctx, account)
	if errTasks != nil {
		a.log.warn("points", fmt.Sprintf("账号 %s 读取新人任务失败：%s", account.label(), asFailure(errTasks).Message))
	} else {
		out.Tasks = tasks
	}
	today := time.Now().In(serverZone).Format("2006-01-02")
	gift, task, consumed, claimedAt, errToday := a.todayTotals(ctx, account, today)
	if errToday != nil {
		a.log.warn("points", fmt.Sprintf("账号 %s 读取今日积分失败：%s", account.label(), asFailure(errToday).Message))
	} else {
		out.Today.GiftPoints = gift
		out.Today.TaskPoints = task
		out.Today.ConsumedPoints = consumed
		if claimedAt != "" {
			out.Today.LastClaimedDate = today
		}
	}
	if includeRecords {
		records, errRecords := a.records(ctx, account, 1, "personal", 50)
		if errRecords != nil {
			out.RecordsError = asFailure(errRecords).Message
		} else {
			out.Records = records
		}
	}
	return out
}

// parseClock reads an "HH:MM" setting. Anything unparsable is reported as not ok
// so the caller can fall back to the default instead of disabling the feature.
func parseClock(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, errHour := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, errMinute := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute, true
}

// claimWindowOpen reports whether today's scheduled claim time has passed.
func claimWindowOpen(schedule string, now time.Time) bool {
	at, ok := parseClock(schedule)
	if !ok {
		at, _ = parseClock(defaultPointsSchedule)
	}
	local := now.In(serverZone)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, serverZone)
	return !local.Before(midnight.Add(at))
}

// pointsScheduleView describes the next and last run for the console.
func (a *app) pointsScheduleView(cfg Config) map[string]any {
	at, _ := parseClock(cfg.PointsScheduleTime)
	local := time.Now().In(serverZone)
	next := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, serverZone).Add(at)
	if !next.After(local) {
		next = next.Add(24 * time.Hour)
	}
	return map[string]any{
		"enabled":  cfg.DailyPointsEnabled,
		"time":     cfg.PointsScheduleTime,
		"window":   claimWindowOpen(cfg.PointsScheduleTime, local),
		"next_run": next.Format(time.RFC3339),
		"now":      local.Format(time.RFC3339),
	}
}

// expiringBucket is one batch of points with a deadline, oldest first.
type expiringBucket struct {
	Points    float64 `json:"points"`
	ExpiresAt string  `json:"expires_at"`
}

// expiringSummary splits the quotas that carry a deadline into "expires within
// the horizon" and "all of them", so the console can warn before points vanish.
func expiringSummary(balance pointsBalance, now time.Time, horizon time.Duration) (soon, total float64, buckets []expiringBucket) {
	limit := now.Add(horizon)
	for _, quota := range balance.Expiring {
		points := quota.RemainingPoints.Float64()
		if points <= 0 {
			continue
		}
		total += points
		buckets = append(buckets, expiringBucket{Points: points, ExpiresAt: quota.ExpiresAt})
		when, ok := parseServerTime(quota.ExpiresAt)
		if ok && !when.After(limit) {
			soon += points
		}
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].ExpiresAt < buckets[j].ExpiresAt })
	if len(buckets) > 4 {
		buckets = buckets[:4]
	}
	return soon, total, buckets
}

// sumTodayRecords turns today's income and expenditure rows into the numbers the
// console shows. Kept pure so the classification stays testable.
func sumTodayRecords(income, expense []pointsRecord) (gift, task, consumed float64, claimedAt string) {
	for _, record := range income {
		switch record.BusinessType {
		case "login_gift":
			gift += record.Amount.Float64()
			if claimedAt == "" {
				claimedAt = record.CreatedAt
			}
		case "onboarding_reward":
			task += record.Amount.Float64()
		}
	}
	for _, record := range expense {
		consumed += record.Amount.Float64()
	}
	return gift, task, consumed, claimedAt
}
