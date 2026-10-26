//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropictokenizer"
)

// fakeKiroCacheRemote 是远端存储的进程内假件：计数调用、可注入故障。
type fakeKiroCacheRemote struct {
	mu      sync.Mutex
	alive   map[[32]byte]time.Duration
	lookups int
	stores  int
	fail    bool
}

func (f *fakeKiroCacheRemote) LookupAlive(_ context.Context, _ uint64, fingerprints [][32]byte) (map[[32]byte]time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.fail {
		return nil, errors.New("remote unavailable")
	}
	out := make(map[[32]byte]time.Duration, len(fingerprints))
	for _, fp := range fingerprints {
		if ttl, ok := f.alive[fp]; ok {
			out[fp] = ttl
		}
	}
	return out, nil
}

func (f *fakeKiroCacheRemote) StoreEntries(_ context.Context, _ uint64, entries []KiroCacheRemoteEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stores++
	if f.fail {
		return errors.New("remote unavailable")
	}
	if f.alive == nil {
		f.alive = make(map[[32]byte]time.Duration, len(entries))
	}
	for _, e := range entries {
		f.alive[e.Fingerprint] = e.TTL
	}
	return nil
}

func (f *fakeKiroCacheRemote) snapshot() (lookups, stores int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, f.stores
}

func (f *fakeKiroCacheRemote) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

// syncKiroCacheDispatchForTest 把远端写入改为同步执行，测试结束后恢复。
func syncKiroCacheDispatchForTest(t *testing.T) {
	t.Helper()
	prev := kiroCacheEmulationAsyncDispatch
	kiroCacheEmulationAsyncDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { kiroCacheEmulationAsyncDispatch = prev })
}

func remoteTestTurnPayload(t *testing.T, msgs ...[2]string) []byte {
	t.Helper()
	ms := make([]any, 0, len(msgs))
	for _, m := range msgs {
		ms = append(ms, map[string]any{"role": m[0], "content": []any{map[string]any{"type": "text", "text": m[1]}}})
	}
	payload := map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   []any{map[string]any{"type": "text", "text": strings.Repeat("You are a coding assistant with detailed instructions. ", 120)}},
		"messages": ms,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func remoteTestProfile(t *testing.T, body []byte) *kiroCacheProfile {
	t.Helper()
	profile, ok := buildKiroCacheProfile(context.Background(), anthropictokenizer.AlgorithmOfficial, body, "claude-sonnet-4-5", 0)
	if !ok {
		t.Fatal("no cacheable profile")
	}
	return profile
}

// 双节点：节点 A 写入远端后，无本地状态的节点 B 通过远端命中；
// 命中回填本地后，远端故障也不影响本节点后续命中（fail-open 不回退已回填状态）。
func TestKiroCacheTrackerSharesStateViaRemote(t *testing.T) {
	syncKiroCacheDispatchForTest(t)
	ctx := context.Background()
	remote := &fakeKiroCacheRemote{}
	nodeA := &kiroCacheTracker{entries: map[uint64]map[[32]byte]kiroCacheEntry{}}
	nodeB := &kiroCacheTracker{entries: map[uint64]map[[32]byte]kiroCacheEntry{}}
	acct := &Account{ID: 90210}
	key := kiroCacheCredentialKey(acct)

	turn1 := remoteTestProfile(t, remoteTestTurnPayload(t, [2]string{"user", strings.Repeat("Please review the architecture and begin the refactor. ", 30)}))
	u1 := nodeA.compute(ctx, remote, key, turn1)
	if u1.CacheReadInputTokens != 0 {
		t.Fatalf("cold start read = %d, want 0", u1.CacheReadInputTokens)
	}
	nodeA.update(remote, key, turn1)
	if _, stores := remote.snapshot(); stores != 1 {
		t.Fatalf("remote stores = %d, want 1", stores)
	}

	turn2 := remoteTestProfile(t, remoteTestTurnPayload(t,
		[2]string{"user", strings.Repeat("Please review the architecture and begin the refactor. ", 30)},
		[2]string{"assistant", strings.Repeat("The refactor plan is ready and the first module is done. ", 30)},
		[2]string{"user", strings.Repeat("Continue with the second module. ", 30)}))
	u2 := nodeB.compute(ctx, remote, key, turn2)
	if u2.CacheReadInputTokens <= 0 {
		t.Fatalf("cross-node read = %d, want > 0", u2.CacheReadInputTokens)
	}
	if u2.CacheReadInputTokens+u2.CacheCreationInputTokens > turn2.totalInputTokens {
		t.Fatalf("usage exceeds total: read=%d creation=%d total=%d",
			u2.CacheReadInputTokens, u2.CacheCreationInputTokens, turn2.totalInputTokens)
	}

	// 回填后，远端故障不影响节点 B 的本地命中。
	remote.setFail(true)
	u3 := nodeB.compute(ctx, remote, key, turn2)
	if u3.CacheReadInputTokens <= 0 {
		t.Fatalf("local hit after backfill = %d, want > 0", u3.CacheReadInputTokens)
	}
}

// 远端全程不可用：fail-open，行为等价纯内存模式（首个请求全量 creation，不崩溃）。
func TestKiroCacheTrackerRemoteFailureFailsOpen(t *testing.T) {
	syncKiroCacheDispatchForTest(t)
	ctx := context.Background()
	remote := &fakeKiroCacheRemote{}
	remote.setFail(true)
	tracker := &kiroCacheTracker{entries: map[uint64]map[[32]byte]kiroCacheEntry{}}
	acct := &Account{ID: 90211}
	key := kiroCacheCredentialKey(acct)

	profile := remoteTestProfile(t, remoteTestTurnPayload(t, [2]string{"user", strings.Repeat("Cold start under remote failure. ", 40)}))
	u := tracker.compute(ctx, remote, key, profile)
	if u.CacheReadInputTokens != 0 {
		t.Fatalf("read under remote failure = %d, want 0", u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens <= 0 {
		t.Fatal("creation should still be counted")
	}
	// commit 在远端故障下不 panic，本地仍写入。
	tracker.update(remote, key, profile)
	u2 := tracker.compute(ctx, remote, key, profile)
	if u2.CacheReadInputTokens <= 0 {
		t.Fatalf("local hit despite remote failure = %d, want > 0", u2.CacheReadInputTokens)
	}
}

// 默认关闭（未注入远端）：plan 流程零远端调用，行为与纯内存一致。
func TestKiroCacheEmulationPlanWithoutRemoteMakesNoRemoteCalls(t *testing.T) {
	syncKiroCacheDispatchForTest(t)
	svc := &GatewayService{}
	group := &Group{
		Platform:                  PlatformKiro,
		KiroCacheEmulationEnabled: true,
		KiroCacheEmulationMode:    KiroCacheEmulationModeUniform,
		KiroCacheEmulationRatio:   1.0,
	}
	NormalizeGroupRuntimeFields(group)
	acct := &Account{ID: 90212, Platform: PlatformKiro}
	body := remoteTestTurnPayload(t, [2]string{"user", strings.Repeat("Default-off request body. ", 60)})

	plan := svc.prepareKiroCacheEmulationUsage(context.Background(), acct, group, body, "claude-sonnet-4-5", 0)
	if plan == nil {
		t.Fatal("plan is nil")
	}
	if plan.remote != nil {
		t.Fatalf("plan.remote = %v, want nil", plan.remote)
	}
	plan.commit()
	if u := plan.result(); u == nil || u.CacheCreationInputTokens <= 0 {
		t.Fatalf("cold start should report creation, got %+v", u)
	}

	// 注入远端后 plan 携带引用，commit 落远端。
	remote := &fakeKiroCacheRemote{}
	svcRemote := &GatewayService{kiroCacheEmulationRemote: remote}
	plan2 := svcRemote.prepareKiroCacheEmulationUsage(context.Background(), acct, group, body, "claude-sonnet-4-5", 0)
	if plan2 == nil || plan2.remote == nil {
		t.Fatal("plan2 should carry remote store")
	}
	plan2.commit()
	if _, stores := remote.snapshot(); stores != 1 {
		t.Fatalf("remote stores = %d, want 1", stores)
	}
}

// 远端写入的内容与 profile 断点一致（指纹、TTL、token）。
func TestKiroCacheTrackerRemoteEntryPayload(t *testing.T) {
	syncKiroCacheDispatchForTest(t)
	remote := &fakeKiroCacheRemote{}
	tracker := &kiroCacheTracker{entries: map[uint64]map[[32]byte]kiroCacheEntry{}}
	acct := &Account{ID: 90213}
	key := kiroCacheCredentialKey(acct)

	profile := remoteTestProfile(t, remoteTestTurnPayload(t, [2]string{"user", fmt.Sprint(strings.Repeat("payload verification body. ", 80))}))
	tracker.update(remote, key, profile)

	remote.mu.Lock()
	defer remote.mu.Unlock()
	breakpoints := profile.cacheableBreakpoints()
	if len(remote.alive) != len(breakpoints) {
		t.Fatalf("stored %d entries, want %d breakpoints", len(remote.alive), len(breakpoints))
	}
	for _, bp := range breakpoints {
		fp := profile.blocks[bp.blockIndex].prefixFingerprint
		if _, ok := remote.alive[fp]; !ok {
			t.Fatalf("breakpoint at block %d not stored remotely", bp.blockIndex)
		}
	}
}

// 防回归：未启用模拟的分组不产生任何远端交互。
func TestKiroCacheEmulationDisabledGroupSkipsRemote(t *testing.T) {
	syncKiroCacheDispatchForTest(t)
	remote := &fakeKiroCacheRemote{}
	svc := &GatewayService{kiroCacheEmulationRemote: remote}
	group := &Group{Platform: PlatformKiro}
	NormalizeGroupRuntimeFields(group)
	acct := &Account{ID: 90214, Platform: PlatformKiro}
	body := remoteTestTurnPayload(t, [2]string{"user", "small request"})

	plan := svc.prepareKiroCacheEmulationUsage(context.Background(), acct, group, body, "claude-sonnet-4-5", 0)
	if plan != nil {
		t.Fatal("plan should be nil for disabled group")
	}
	if lookups, stores := remote.snapshot(); lookups != 0 || stores != 0 {
		t.Fatalf("remote interaction for disabled group: lookups=%d stores=%d", lookups, stores)
	}
}
