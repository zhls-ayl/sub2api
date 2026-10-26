//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newKiroCacheEmulationTestStore(t *testing.T) (service.KiroCacheEmulationRemoteStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewKiroCacheEmulationRemoteStore(rdb), mr
}

func kiroCacheEmulationFP(seed byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = seed + byte(i)
	}
	return out
}

func TestKiroCacheEmulationStoreNilClientUnavailable(t *testing.T) {
	s := NewKiroCacheEmulationRemoteStore(nil)
	if _, err := s.LookupAlive(context.Background(), 1, [][32]byte{kiroCacheEmulationFP(1)}); err != ErrKiroCacheEmulationStoreUnavailable {
		t.Fatalf("LookupAlive nil client err = %v, want ErrKiroCacheEmulationStoreUnavailable", err)
	}
	if err := s.StoreEntries(context.Background(), 1, []service.KiroCacheRemoteEntry{{Fingerprint: kiroCacheEmulationFP(1), Tokens: 10, TTL: time.Minute}}); err != ErrKiroCacheEmulationStoreUnavailable {
		t.Fatalf("StoreEntries nil client err = %v, want ErrKiroCacheEmulationStoreUnavailable", err)
	}
}

func TestKiroCacheEmulationStoreRoundTripAndTTL(t *testing.T) {
	s, mr := newKiroCacheEmulationTestStore(t)
	ctx := context.Background()

	entries := []service.KiroCacheRemoteEntry{
		{Fingerprint: kiroCacheEmulationFP(1), Tokens: 1200, TTL: 5 * time.Minute},
		{Fingerprint: kiroCacheEmulationFP(2), Tokens: 8000, TTL: time.Hour},
	}
	if err := s.StoreEntries(ctx, 42, entries); err != nil {
		t.Fatalf("StoreEntries: %v", err)
	}

	alive, err := s.LookupAlive(ctx, 42, [][32]byte{kiroCacheEmulationFP(1), kiroCacheEmulationFP(2), kiroCacheEmulationFP(3)})
	if err != nil {
		t.Fatalf("LookupAlive: %v", err)
	}
	if len(alive) != 2 {
		t.Fatalf("alive = %d entries, want 2", len(alive))
	}
	if got := alive[kiroCacheEmulationFP(1)]; got != 5*time.Minute {
		t.Fatalf("fp1 ttl = %v, want 5m", got)
	}
	if got := alive[kiroCacheEmulationFP(2)]; got != time.Hour {
		t.Fatalf("fp2 ttl = %v, want 1h", got)
	}

	// key 自身带 EX：fast-forward 后过期即消失。
	mr.FastForward(6 * time.Minute)
	alive, err = s.LookupAlive(ctx, 42, [][32]byte{kiroCacheEmulationFP(1), kiroCacheEmulationFP(2)})
	if err != nil {
		t.Fatalf("LookupAlive after fast-forward: %v", err)
	}
	if _, ok := alive[kiroCacheEmulationFP(1)]; ok {
		t.Fatal("fp1 should have expired")
	}
	if _, ok := alive[kiroCacheEmulationFP(2)]; !ok {
		t.Fatal("fp2 should still be alive")
	}
}

func TestKiroCacheEmulationStoreOverwriteRenewsTTL(t *testing.T) {
	s, mr := newKiroCacheEmulationTestStore(t)
	ctx := context.Background()

	if err := s.StoreEntries(ctx, 7, []service.KiroCacheRemoteEntry{{Fingerprint: kiroCacheEmulationFP(9), Tokens: 10, TTL: 5 * time.Minute}}); err != nil {
		t.Fatalf("StoreEntries: %v", err)
	}
	mr.FastForward(4 * time.Minute)
	// 幂等覆盖写：EX 被重置，实现滑动续期。
	if err := s.StoreEntries(ctx, 7, []service.KiroCacheRemoteEntry{{Fingerprint: kiroCacheEmulationFP(9), Tokens: 10, TTL: 5 * time.Minute}}); err != nil {
		t.Fatalf("StoreEntries rewrite: %v", err)
	}
	mr.FastForward(4 * time.Minute)
	alive, err := s.LookupAlive(ctx, 7, [][32]byte{kiroCacheEmulationFP(9)})
	if err != nil {
		t.Fatalf("LookupAlive: %v", err)
	}
	if _, ok := alive[kiroCacheEmulationFP(9)]; !ok {
		t.Fatal("entry should survive via TTL reset")
	}
}

func TestKiroCacheEmulationStoreEmptyInputs(t *testing.T) {
	s, _ := newKiroCacheEmulationTestStore(t)
	ctx := context.Background()
	alive, err := s.LookupAlive(ctx, 1, nil)
	if err != nil || len(alive) != 0 {
		t.Fatalf("LookupAlive(nil) = %v, %v; want empty, nil", alive, err)
	}
	if err := s.StoreEntries(ctx, 1, nil); err != nil {
		t.Fatalf("StoreEntries(nil): %v", err)
	}
}

func TestKiroCacheEmulationStoreServerDownFailsGracefully(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewKiroCacheEmulationRemoteStore(rdb)
	mr.Close()

	if _, err := s.LookupAlive(context.Background(), 1, [][32]byte{kiroCacheEmulationFP(1)}); err == nil {
		t.Fatal("LookupAlive on closed server should error")
	}
	if err := s.StoreEntries(context.Background(), 1, []service.KiroCacheRemoteEntry{{Fingerprint: kiroCacheEmulationFP(1), Tokens: 1, TTL: time.Minute}}); err == nil {
		t.Fatal("StoreEntries on closed server should error")
	}
}
