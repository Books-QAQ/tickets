package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Reranker 精排接口（§7.4 精排行；10⁵ 级 near-miss 变多，rerank 是必需项）
type RerankCandidate struct {
	ChunkID     int64
	Text        string
	FusionScore float64
}

type Reranker interface {
	Name() string
	Rerank(ctx context.Context, question string, cands []RerankCandidate, topN int) ([]RerankCandidate, error)
}

// FusionReranker 降级实现：原样返回融合顺序（"不可用则回退融合分"是默认行为）。
// 调用方负责计数（rerank_fallback_total），**不允许静默降级**。
type FusionReranker struct{}

func (FusionReranker) Name() string { return "fusion-fallback" }

func (FusionReranker) Rerank(_ context.Context, _ string, cands []RerankCandidate, topN int) ([]RerankCandidate, error) {
	if topN > 0 && len(cands) > topN {
		return cands[:topN], nil
	}
	return cands, nil
}

// HTTPReranker OpenAI 兼容的 /rerank 客户端（如 BGE-reranker-v2-m3）。
// 供应商未定（19.10）时不要配置 → 自动走 FusionReranker。
type HTTPReranker struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func (r *HTTPReranker) Name() string { return "http:" + r.Model }

func (r *HTTPReranker) Rerank(ctx context.Context, question string, cands []RerankCandidate, topN int) ([]RerankCandidate, error) {
	if len(cands) == 0 {
		return cands, nil
	}
	if r.APIKey == "" || r.BaseURL == "" {
		return nil, fmt.Errorf("rerank: 未配置供应商（19.10 未决策）")
	}
	docs := make([]string, 0, len(cands))
	for _, c := range cands {
		docs = append(docs, c.Text)
	}
	body, _ := json.Marshal(map[string]any{
		"model": r.Model, "query": question, "documents": docs, "top_n": topN,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.APIKey)

	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank 上游 HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("rerank 返回空结果")
	}
	out := make([]RerankCandidate, 0, len(parsed.Results))
	for _, item := range parsed.Results {
		if item.Index < 0 || item.Index >= len(cands) {
			continue
		}
		c := cands[item.Index]
		c.FusionScore = item.RelevanceScore
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FusionScore > out[j].FusionScore })
	return out, nil
}
