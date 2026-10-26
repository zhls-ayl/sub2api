package service

import (
	"context"
	"time"
)

// KiroCacheRemoteEntry 是缓存模拟 tracker 写入远端存储的单个前缀条目。
// Tokens 仅供调试与本地回填对齐——计量输出从不读取存储的 token 值。
type KiroCacheRemoteEntry struct {
	Fingerprint [32]byte
	Tokens      int
	TTL         time.Duration
}

// KiroCacheEmulationRemoteStore 是 Kiro 缓存模拟 tracker 的跨节点共享存储。
//
// 语义约定（实现见 internal/pkg/kirocachemeter）：
//   - 存活期的唯一真值是存储侧 TTL（Redis key 的 EX），值内不携带应用层
//     过期时间戳，避免多实例时钟不一致与"EXPIRE 续期与存储时间戳脱节"问题；
//   - 写入为幂等覆盖，重复写同一前缀即滑动续期；
//   - LookupAlive 只返回仍存活的指纹；缺失与过期不可区分，调用方语义上
//     等同未命中；
//   - 任一方法出错时调用方按本地内存结果继续（fail-open）。
type KiroCacheEmulationRemoteStore interface {
	// LookupAlive 批量查询指纹存活状态，返回仍存活的指纹与其 TTL。
	LookupAlive(ctx context.Context, accountKey uint64, fingerprints [][32]byte) (map[[32]byte]time.Duration, error)
	// StoreEntries 批量幂等写入指纹条目，TTL 即存活期。
	StoreEntries(ctx context.Context, accountKey uint64, entries []KiroCacheRemoteEntry) error
}

// kiroCacheEmulationAsyncDispatch 把远端写入派发到后台执行，默认 go 关键字。
// 测试可替换为同步执行，保证断言前写入已落地。
var kiroCacheEmulationAsyncDispatch = func(fn func()) { go fn() }

// kiroCacheEmulationRemoteWriteTimeout 是远端写入的后台超时，独立于请求
// context（commit 发生在响应路径上，不应被客户端取消传播）。
const kiroCacheEmulationRemoteWriteTimeout = 1500 * time.Millisecond

// kiroCacheEmulationRemoteStore 返回注入的远端共享存储；未配置（默认关闭）
// 返回 nil，tracker 按纯内存模式工作。
func (s *GatewayService) kiroCacheEmulationRemoteStore() KiroCacheEmulationRemoteStore {
	if s == nil {
		return nil
	}
	return s.kiroCacheEmulationRemote
}
