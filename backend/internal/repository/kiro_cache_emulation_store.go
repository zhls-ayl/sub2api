package repository

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Kiro 缓存模拟 tracker 的 Redis 共享存储：多实例部署共享前缀命中状态，
// key 自身 TTL 即存活真值（见 service.KiroCacheEmulationRemoteStore 的语义约定）。

const (
	kiroCacheEmulationKeyPrefix   = "kiro:cacheemul:v1:e:"
	kiroCacheEmulationOpTimeout   = 1500 * time.Millisecond
	kiroCacheEmulationLogThrottle = 10 * time.Second
)

// ErrKiroCacheEmulationStoreUnavailable 表示未配置 Redis client，
// 调用方应按纯内存模式继续。
var ErrKiroCacheEmulationStoreUnavailable = errors.New("kiro cache emulation store unavailable")

type kiroCacheEmulationEntryValue struct {
	Tokens int   `json:"tokens"`
	TTLSec int64 `json:"ttl"`
}

// KiroCacheEmulationStore 是 service.KiroCacheEmulationRemoteStore 的 Redis 实现。
type KiroCacheEmulationStore struct {
	rdb     *redis.Client
	lastLog atomic.Int64
}

// NewKiroCacheEmulationRemoteStore 返回 Redis 存储；rdb 为 nil 时方法返回
// ErrKiroCacheEmulationStoreUnavailable。
func NewKiroCacheEmulationRemoteStore(rdb *redis.Client) service.KiroCacheEmulationRemoteStore {
	return &KiroCacheEmulationStore{rdb: rdb}
}

func kiroCacheEmulationEntryKey(accountKey uint64, fp [32]byte) string {
	return kiroCacheEmulationKeyPrefix + strconv.FormatUint(accountKey, 10) + ":" + hex.EncodeToString(fp[:])
}

// LookupAlive 批量返回仍存活（key 存在且未过期）的指纹及其 TTL。
// 过期或缺失的指纹不出现在结果里。
func (s *KiroCacheEmulationStore) LookupAlive(ctx context.Context, accountKey uint64, fingerprints [][32]byte) (map[[32]byte]time.Duration, error) {
	if s == nil || s.rdb == nil {
		return nil, ErrKiroCacheEmulationStoreUnavailable
	}
	if len(fingerprints) == 0 {
		return map[[32]byte]time.Duration{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, kiroCacheEmulationOpTimeout)
	defer cancel()

	keys := make([]string, len(fingerprints))
	for i, fp := range fingerprints {
		keys[i] = kiroCacheEmulationEntryKey(accountKey, fp)
	}
	values, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		s.throttledWarn("kiro cache emulation redis lookup failed", err)
		return nil, err
	}
	alive := make(map[[32]byte]time.Duration, len(values))
	for i, raw := range values {
		if raw == nil {
			continue
		}
		str, ok := raw.(string)
		if !ok {
			continue
		}
		var v kiroCacheEmulationEntryValue
		if json.Unmarshal([]byte(str), &v) != nil {
			continue
		}
		ttl := time.Duration(v.TTLSec) * time.Second
		if ttl <= 0 {
			continue
		}
		alive[fingerprints[i]] = ttl
	}
	return alive, nil
}

// StoreEntries 批量写入（幂等覆盖）指纹条目，EX 即存活期。
// 同一前缀被再次写入时 EX 重置，天然实现滑动续期。
func (s *KiroCacheEmulationStore) StoreEntries(ctx context.Context, accountKey uint64, entries []service.KiroCacheRemoteEntry) error {
	if s == nil || s.rdb == nil {
		return ErrKiroCacheEmulationStoreUnavailable
	}
	if len(entries) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, kiroCacheEmulationOpTimeout)
	defer cancel()

	pipe := s.rdb.Pipeline()
	for _, e := range entries {
		if e.TTL <= 0 {
			continue
		}
		value, err := json.Marshal(kiroCacheEmulationEntryValue{Tokens: e.Tokens, TTLSec: int64(e.TTL.Seconds())})
		if err != nil {
			continue
		}
		pipe.Set(ctx, kiroCacheEmulationEntryKey(accountKey, e.Fingerprint), value, e.TTL)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		s.throttledWarn("kiro cache emulation redis store failed", err)
	}
	return err
}

func (s *KiroCacheEmulationStore) throttledWarn(msg string, err error) {
	now := time.Now().UnixNano()
	last := s.lastLog.Load()
	if now-last < int64(kiroCacheEmulationLogThrottle) {
		return
	}
	if s.lastLog.CompareAndSwap(last, now) {
		slog.Warn(msg, "error", err)
	}
}
