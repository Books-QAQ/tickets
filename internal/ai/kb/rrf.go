package kb

import "sort"

// RRF（Reciprocal Rank Fusion）：score = Σ 1/(k + rank)，k=60（§7.4）
// 融合位置固定在 Go 侧（确定性、可评测、单一计数点），不交给 Qdrant —— 见 ADR-10 被否方案。
const RRFK = 60

// Fuse 融合多路有序结果。各路结果按各自顺序参与排名（rank 从 1 开始）。
func Fuse(lists ...[]Hit) []Hit {
	acc := map[int64]float64{}
	for _, list := range lists {
		for i, h := range list {
			acc[h.ChunkID] += 1.0 / float64(RRFK+i+1)
		}
	}
	out := make([]Hit, 0, len(acc))
	for id, s := range acc {
		out = append(out, Hit{ChunkID: id, Score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ChunkID < out[j].ChunkID
		}
		return out[i].Score > out[j].Score
	})
	return out
}
