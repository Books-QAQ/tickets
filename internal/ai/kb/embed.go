package kb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// Embedder 向量化接口（§11.3：无 key 时必须能降级，且降级要可见）
type Embedder interface {
	Name() string
	Dim() int
	// Semantic 是否具备真实语义（false = 词面替身，检索结果必须标 vector_mode=ngram + degraded.vector）
	Semantic() bool
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// FakeEmbedder 确定性哈希向量（hashing trick + L2 归一化）。
//
// 用途：在没有 embedding 供应商 key 时打通「入库 → Qdrant → 检索」全链路（M1 验收需要），
// 它**不表达语义**（只表达词面重合），因此：
//   - 可以验证契约、过滤、RRF、门槛、失效路径；
//   - **不能**用来证明检索质量——真实检索质量必须换真实 embedding 重测（见 19.1 与 M1 滞留清单）。
type FakeEmbedder struct{ DimN int }

func (f FakeEmbedder) dim() int {
	if f.DimN <= 0 {
		return 1024
	}
	return f.DimN
}

func (f FakeEmbedder) Name() string { return fmt.Sprintf("fake-hash-%d", f.dim()) }
func (f FakeEmbedder) Dim() int     { return f.dim() }

// Semantic 词面哈希向量不具备真实语义 → false
func (f FakeEmbedder) Semantic() bool { return false }

func (f FakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	dim := f.dim()
	for _, t := range texts {
		vec := make([]float32, dim)
		for _, tok := range Tokenize(t) {
			sum := sha256.Sum256([]byte(tok))
			idx := int(binary.BigEndian.Uint32(sum[:4]) % uint32(dim))
			sign := float32(1)
			if sum[4]%2 == 1 {
				sign = -1
			}
			vec[idx] += sign
		}
		var norm float64
		for _, v := range vec {
			norm += float64(v) * float64(v)
		}
		if norm > 0 {
			n := float32(math.Sqrt(norm))
			for i := range vec {
				vec[i] /= n
			}
		}
		out = append(out, vec)
	}
	return out, nil
}

// OpenAIEmbedder OpenAI 兼容的 embeddings 客户端（key 只在 Go 侧，见 ADR-1）
type OpenAIEmbedder struct {
	BaseURL string // 例：https://api.siliconflow.cn/v1（BGE-M3）或自建网关
	APIKey  string
	Model   string
	DimN    int
	Client  *http.Client
}

func (e *OpenAIEmbedder) httpClient() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (e *OpenAIEmbedder) Name() string { return "openai:" + e.Model }

// Semantic 真实供应商 embedding → true
func (e *OpenAIEmbedder) Semantic() bool { return true }
func (e *OpenAIEmbedder) Dim() int {
	if e.DimN <= 0 {
		return 1024
	}
	return e.DimN
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.APIKey == "" {
		return nil, fmt.Errorf("embedding: 未配置 API key")
	}
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.APIKey)

	resp, err := e.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embedding 响应解析失败: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("embedding 上游错误: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embedding 返回条数不匹配: got %d want %d", len(parsed.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embedding 返回 index 越界: %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}
