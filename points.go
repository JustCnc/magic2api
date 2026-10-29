package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	return balance, nil
}

// records lists the point ledger.
func (a *app) records(ctx context.Context, account *credential, filterType int, filterRange string, pageSize int) ([]pointsRecord, error) {
	if filterType == 0 {
		filterType = 3 // Expenditure
	}
	if filterRange == "" {
		filterRange = "personal"
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	body, _ := json.Marshal(map[string]any{
		"date":         "",
		"filter_type":  filterType,
		"filter_range": filterRange,
		"page":         1,
		"page_size":    pageSize,
	})
	data, err := a.upstream().callAsset(ctx, http.MethodPost, "/api/v1/quota/points/records/queries", account.Authorization, account.OrganizationCode, body)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Total flexInt        `json:"total"`
		List  []pointsRecord `json:"list"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("解析积分流水失败：%w", err)
	}
	return payload.List, nil
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

// attachRequestPoints matches a finished request against the ledger and writes
// the consumed points back into the request log.
func (a *app) attachRequestPoints(seq int64, account *credential, model string, started, finished time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The ledger row is written asynchronously; give it a moment and retry.
	for attempt := 0; attempt < 4; attempt++ {
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
		records, err := a.records(ctx, account, 3, "personal", 30)
		if err != nil {
			continue
		}
		total, matched := a.claimModelRecords(account.ID, records, model, started, finished)
		if matched {
			if total != 0 {
				a.requests.setPoints(seq, total)
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
			if cfg.DailyPointsEnabled {
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
	if includeRecords {
		records, errRecords := a.records(ctx, account, 1, "personal", 50)
		if errRecords != nil {
			out.RecordsError = asFailure(errRecords).Message
		} else {
			out.Records = records
			today := time.Now().In(serverZone).Format("2006-01-02")
			for _, record := range records {
				created, ok := parseServerTime(record.CreatedAt)
				if !ok || created.In(serverZone).Format("2006-01-02") != today {
					continue
				}
				switch record.BusinessType {
				case "login_gift":
					out.Today.GiftPoints += record.Amount.Float64()
					out.Today.LastClaimedDate = today
				case "onboarding_reward":
					out.Today.TaskPoints += record.Amount.Float64()
				case "magic_model_consume":
					out.Today.ConsumedPoints += record.Amount.Float64()
				}
			}
		}
	}
	return out
}
