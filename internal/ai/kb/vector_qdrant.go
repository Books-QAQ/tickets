package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// VectorRetriever 向量召回接口 —— **ADR-10 的演进插槽**：
// 现在是 Qdrant 实现；若将来自研/换引擎，只需新增一个实现，调用点一行不改。
type VectorRetriever interface {
	Search(ctx context.Context, vec []float32, f Filter, topK int) ([]VectorHit, error)
}

type VectorHit struct {
	ChunkID int64
	Cos     float64 // 余弦相似度（阈值判定用它，不是 RRF 分数 —— §7.5）
}

// QPoint 入库点
type QPoint struct {
	ChunkID   int64
	Vector    []float32
	Payload   map[string]any
}

// Qdrant 用 REST（零新增依赖；gRPC 更快但需引客户端库，10⁵ 规模 REST 足够）
type Qdrant struct {
	BaseURL    string // http://127.0.0.1:16333（容器内为 http://qdrant:6333）
	APIKey     string
	Collection string
	Client     *http.Client
}

func (q *Qdrant) httpClient() *http.Client {
	if q.Client != nil {
		return q.Client
	}
	return &http.Client{Timeout: 8 * time.Second}
}

func (q *Qdrant) do(ctx context.Context, method, path string, body any, out any) error {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if q.APIKey != "" {
		req.Header.Set("api-key", q.APIKey)
	}
	resp, err := q.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw := json.NewDecoder(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Status struct {
				Error string `json:"error"`
			} `json:"status"`
		}
		_ = raw.Decode(&e)
		return fmt.Errorf("qdrant %s %s: HTTP %d %s", method, path, resp.StatusCode, e.Status.Error)
	}
	if out == nil {
		return nil
	}
	return raw.Decode(out)
}

// EnsureCollection 建集合（已存在则忽略）；dim 必须与 embedding 维度一致
func (q *Qdrant) EnsureCollection(ctx context.Context, dim int) error {
	body := map[string]any{"vectors": map[string]any{"size": dim, "distance": "Cosine"}}
	err := q.do(ctx, http.MethodPut, "/collections/"+q.Collection, body, nil)
	if err != nil {
		// 已存在时 Qdrant 返回 409/400；视为成功
		if containsAny(err.Error(), "already exists", "409", "400") {
			return nil
		}
		return err
	}
	return nil
}

// CreatePayloadIndexes 建 payload 索引（检索过滤只用 payload，不 JOIN MySQL —— §13.1）
func (q *Qdrant) CreatePayloadIndexes(ctx context.Context) error {
	fields := []string{"category", "form", "sub_scenario", "capability", "visibility", "scope_kind", "scope_ref"}
	for _, f := range fields {
		body := map[string]any{"field_name": f, "field_schema": "keyword"}
		if err := q.do(ctx, http.MethodPut, "/collections/"+q.Collection+"/index?wait=true", body, nil); err != nil {
			return fmt.Errorf("建 payload 索引 %s 失败: %w", f, err)
		}
	}
	// 时效用数值比较：expire_at_ts（null 约定为 253402300799 = 9999 年）
	if err := q.do(ctx, http.MethodPut, "/collections/"+q.Collection+"/index?wait=true",
		map[string]any{"field_name": "expire_at_ts", "field_schema": "integer"}, nil); err != nil {
		return err
	}
	return nil
}

func (q *Qdrant) Upsert(ctx context.Context, points []QPoint) error {
	if len(points) == 0 {
		return nil
	}
	pts := make([]map[string]any, 0, len(points))
	for _, p := range points {
		pts = append(pts, map[string]any{"id": p.ChunkID, "vector": p.Vector, "payload": p.Payload})
	}
	return q.do(ctx, http.MethodPut, "/collections/"+q.Collection+"/points?wait=true", map[string]any{"points": pts}, nil)
}

func (q *Qdrant) DeleteByChunkIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return q.do(ctx, http.MethodPost, "/collections/"+q.Collection+"/points/delete?wait=true",
		map[string]any{"points": ids}, nil)
}

// SetPayloadCapability 台账翻转时只更新 payload 的 capability（不重嵌、不动向量 —— §13.1）
func (q *Qdrant) SetPayloadCapability(ctx context.Context, ids []int64, capability string) error {
	if len(ids) == 0 {
		return nil
	}
	return q.do(ctx, http.MethodPost, "/collections/"+q.Collection+"/points/payload?wait=true",
		map[string]any{"payload": map[string]any{"capability": capability}, "points": ids}, nil)
}

func (q *Qdrant) Count(ctx context.Context) (int, error) {
	var out struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := q.do(ctx, http.MethodPost, "/collections/"+q.Collection+"/points/count", map[string]any{"exact": true}, &out); err != nil {
		return 0, err
	}
	return out.Result.Count, nil
}

func (q *Qdrant) Ready(ctx context.Context) bool {
	return q.do(ctx, http.MethodGet, "/collections", nil, nil) == nil
}

type vectorHitPayload struct {
	ChunkID int64 `json:"chunk_id"`
}

// buildFilter 把 Filter 翻成 Qdrant filter（含时效：expire_at_ts >= now）
func buildFilter(f Filter, nowTS int64) map[string]any {
	var must []map[string]any
	if f.Category != "" {
		must = append(must, matchCond("category", f.Category))
	}
	for c := range f.Capabilities {
		must = append(must, matchCond("capability", string(c)))
	}
	if len(f.Visibility) == 1 {
		must = append(must, matchCond("visibility", f.Visibility[0]))
	} else if len(f.Visibility) > 1 {
		should := make([]map[string]any, 0, len(f.Visibility))
		for _, v := range f.Visibility {
			should = append(should, matchCond("visibility", v))
		}
		must = append(must, map[string]any{"should": should})
	}
	if f.ScopeKind != "" {
		must = append(must, matchCond("scope_kind", f.ScopeKind))
	}
	if f.ScopeRef != "" {
		must = append(must, matchCond("scope_ref", f.ScopeRef))
	}
	must = append(must, map[string]any{"key": "expire_at_ts", "range": map[string]any{"gte": nowTS}})
	return map[string]any{"must": must}
}

func matchCond(key, value string) map[string]any {
	return map[string]any{"key": key, "match": map[string]any{"value": value}}
}

// Search 过滤式 ANN（payload 预过滤；两级检索的第一级由 Filter 表达）
func (q *Qdrant) Search(ctx context.Context, vec []float32, f Filter, topK int) ([]VectorHit, error) {
	body := map[string]any{
		"query":        vec,
		"limit":        topK,
		"with_payload": true,
		"filter":       buildFilter(f, time.Now().Unix()),
	}
	var out struct {
		Result struct {
			Points []struct {
				ID      json.Number     `json:"id"`
				Score   float64         `json:"score"`
				Payload vectorHitPayload `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := q.do(ctx, http.MethodPost, "/collections/"+q.Collection+"/points/query", body, &out); err != nil {
		return nil, err
	}
	hits := make([]VectorHit, 0, len(out.Result.Points))
	for _, p := range out.Result.Points {
		id := p.Payload.ChunkID
		if id == 0 {
			if v, err := p.ID.Int64(); err == nil {
				id = v
			}
		}
		hits = append(hits, VectorHit{ChunkID: id, Cos: p.Score})
	}
	return hits, nil
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}
