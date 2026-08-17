package worker

import (
	"testing"

	"github.com/Books-QAQ/tickets/internal/cache"
)

func TestPurchaseShardDeterministic(t *testing.T) {
	// 核心性质：同一座位必须始终路由到同一分片，保证同座位串行处理
	const workerCount = 4
	for _, seatID := range []int32{1, 2, 3, 4, 5, 100, 9999} {
		first := PurchaseShard(seatID, workerCount)
		for i := 0; i < 100; i++ {
			if got := PurchaseShard(seatID, workerCount); got != first {
				t.Fatalf("seat %d: shard not deterministic: %d != %d", seatID, got, first)
			}
		}
	}
}

func TestPurchaseShardRange(t *testing.T) {
	// 分片号必须落在 [0, workerCount)
	const workerCount = 4
	for seatID := int32(1); seatID <= 1000; seatID++ {
		if got := PurchaseShard(seatID, workerCount); got < 0 || got >= workerCount {
			t.Fatalf("seat %d: shard %d out of range", seatID, got)
		}
	}
}

func TestPurchaseShardDistributesAcrossWorkers(t *testing.T) {
	// 连续座位应分散到不同分片（避免热点集中到单 worker）
	const workerCount = 4
	seen := make(map[int]bool)
	for seatID := int32(1); seatID <= 4; seatID++ {
		seen[PurchaseShard(seatID, workerCount)] = true
	}
	if len(seen) != workerCount {
		t.Fatalf("expected seats 1..4 to spread across %d shards, got %d distinct shards", workerCount, len(seen))
	}
}

func TestPurchaseShardFallbackToSingle(t *testing.T) {
	// workerCount <= 0 时回退为单 worker（shard 恒为 0），保证向后兼容
	for _, wc := range []int{0, -1, -100} {
		if got := PurchaseShard(42, wc); got != 0 {
			t.Fatalf("workerCount %d: expected shard 0, got %d", wc, got)
		}
	}
}

func TestPurchaseQueueKeyForShard(t *testing.T) {
	if got := cache.PurchaseQueueKeyForShard(0); got != "queue:purchase:0" {
		t.Fatalf("shard 0: got %q", got)
	}
	if got := cache.PurchaseQueueKeyForShard(3); got != "queue:purchase:3" {
		t.Fatalf("shard 3: got %q", got)
	}
}
