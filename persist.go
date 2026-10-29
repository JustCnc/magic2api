package main

import (
	"context"
	"fmt"
	"time"
)

// The plugin keeps three state files in <data_dir>:
//
//	accounts.json  accounts plus their health and rate-limit state
//	points.json    last known balances, so the console and the router start warm
//	runtime.json   counters, the console's request/log tail, claimed ledger rows
//
// Everything that could be rebuilt from upstream is persisted anyway: a warm
// start avoids a burst of asset calls and the "everything looks brand new" gap
// right after a restart.

const (
	runtimeFileName = "runtime.json"
	pointsFileName  = "points.json"
	// persistInterval is how often dirty in-memory state is flushed. A crash
	// loses at most this much of the console's history.
	persistInterval = 30 * time.Second
	// maxPersistedClaims bounds runtime.json; the claim set already resets in
	// memory when it grows past its own limit.
	maxPersistedClaims = 4000
)

type runtimeFile struct {
	Version  int              `json:"version"`
	Metrics  map[string]int64 `json:"metrics"`
	Requests []requestEntry   `json:"requests"`
	Logs     []logEntry       `json:"logs"`
	Claims   []string         `json:"claims"`
}

// markDirty flags account state worth flushing on the next persist tick.
func (a *app) markDirty() { a.dirty.Store(true) }

func (a *app) loadRuntimeState() {
	store := a.stateStore()
	if store == nil {
		return
	}
	var file runtimeFile
	ok, err := store.readJSON(runtimeFileName, &file)
	if err != nil {
		a.log.warn("state", "读取 runtime.json 失败："+err.Error())
	} else if ok {
		a.metrics.restore(file.Metrics)
		a.requests.restore(file.Requests)
		a.log.restore(file.Logs)
		restoreClaimedRecords(file.Claims)
		a.log.info("state", fmt.Sprintf("已恢复运行状态：%d 条请求日志、%d 条运行日志、%d 个已认领流水",
			len(file.Requests), len(file.Logs), len(file.Claims)))
	}
	a.loadPointsCache()
}

// persistLoop flushes dirty state periodically; shutdown flushes once more.
func (a *app) persistLoop(ctx context.Context) {
	ticker := time.NewTicker(persistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.flushState()
		}
	}
}

// flushState writes only the files whose content actually moved.
func (a *app) flushState() {
	if a.dirty.Swap(false) {
		a.persistAccounts()
	}
	if a.observedDirty() {
		a.saveRuntimeState()
	}
	if a.pointsDirty.Swap(false) {
		a.savePointsCache()
	}
}

// observedDirty merges the lazy dirty flags of the in-memory rings.
func (a *app) observedDirty() bool {
	dirty := a.log.takeDirty()
	if a.requests.takeDirty() {
		dirty = true
	}
	if a.metrics.takeDirty() {
		dirty = true
	}
	if claimedRecords.takeDirty() {
		dirty = true
	}
	return dirty
}

func (a *app) saveRuntimeState() {
	store := a.stateStore()
	if store == nil {
		return
	}
	file := runtimeFile{
		Version:  1,
		Metrics:  a.metrics.values(),
		Requests: a.requests.snapshot(),
		Logs:     a.log.snapshot(),
		Claims:   claimedRecordIDs(maxPersistedClaims),
	}
	if err := store.writeJSON(runtimeFileName, file); err != nil {
		a.log.warn("state", "写入 runtime.json 失败："+err.Error())
	}
}
