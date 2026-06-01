package proxy

import (
	"sync"
	"testing"
)

// accountRefreshLock 是 token 刷新锁 per-account 化改造的核心：它必须为同一
// accountID 始终返回同一把 *sync.Mutex（保证同账号刷新互斥 + double-check 去重），
// 为不同 accountID 返回不同的锁（保证不同账号刷新互不阻塞，消除全局串行瓶颈）。
// 这些测试直接验证该不变量，不依赖网络刷新桩。

func TestAccountRefreshLockSameIDReturnsSameLock(t *testing.T) {
	h := &Handler{}
	a := h.accountRefreshLock("acct-1")
	b := h.accountRefreshLock("acct-1")
	if a != b {
		t.Fatalf("expected same lock instance for identical accountID, got %p and %p", a, b)
	}
}

func TestAccountRefreshLockDifferentIDsReturnDifferentLocks(t *testing.T) {
	h := &Handler{}
	a := h.accountRefreshLock("acct-1")
	b := h.accountRefreshLock("acct-2")
	if a == b {
		t.Fatalf("expected distinct lock instances for different accountIDs, both were %p", a)
	}
	// 不同账号的锁互不影响：锁住一个不应阻塞另一个。
	a.Lock()
	defer a.Unlock()
	if !b.TryLock() {
		t.Fatalf("locking acct-1 must not block acct-2's lock")
	}
	b.Unlock()
}

// TestAccountRefreshLockConcurrentSameIDIsStable 验证 LoadOrStore 的原子性：
// 大量 goroutine 并发请求同一 accountID 的锁，必须全部拿到同一把锁实例，
// 否则同账号互斥失效（两个请求可能各持一把锁同时刷新）。配合 -race 运行。
func TestAccountRefreshLockConcurrentSameIDIsStable(t *testing.T) {
	h := &Handler{}
	const n = 64
	results := make([]*sync.Mutex, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = h.accountRefreshLock("same")
		}(i)
	}
	wg.Wait()

	first := results[0]
	if first == nil {
		t.Fatal("got nil lock")
	}
	for i, got := range results {
		if got != first {
			t.Fatalf("goroutine %d got a different lock instance (%p != %p)", i, got, first)
		}
	}
}
