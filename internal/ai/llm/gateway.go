// Package llm LLM 网关：编排层只与本包对话，供应商密钥只存在于 Go 进程（ADR-1）。
// 提供两种 provider：
//   - openai：OpenAI 兼容 /chat/completions（真实供应商，19.1 决策后配置）
//   - mock：确定性假模型，用于无 key 环境下打通链路与验收（**不产生真实答案**）
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type ChatResponse struct {
	Content      string `json:"content"`
	PromptTokens int    `json:"prompt_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
	Provider     string `json:"provider"`
	Delegated    bool   `json:"delegated"` // 是否走了兜底（degraded.llm）
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error)
}

// —— OpenAI 兼容 ——

type OpenAIProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewOpenAIProvider(baseURL, apiKey, model string) *OpenAIProvider {
	return &OpenAIProvider{BaseURL: strings.TrimSuffix(baseURL, "/"), APIKey: apiKey, Model: model,
		Client: &http.Client{Timeout: 20 * time.Second}}
}

func (p *OpenAIProvider) Name() string { return "openai:" + p.Model }

func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llm 上游 HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("llm 返回空 choices")
	}
	return &ChatResponse{
		Content:      parsed.Choices[0].Message.Content,
		PromptTokens: parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
		TotalTokens:  parsed.Usage.TotalTokens,
		Provider:     p.Name(),
	}, nil
}

func (p *OpenAIProvider) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llm 上游 HTTP %d", resp.StatusCode)
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content == "" {
				continue
			}
			sb.WriteString(c.Delta.Content)
			if err := onDelta(c.Delta.Content); err != nil {
				return nil, err
			}
		}
	}
	out := sb.String()
	return &ChatResponse{Content: out, Provider: p.Name(), TotalTokens: utf8.RuneCountInString(out)}, scanner.Err()
}

// —— 确定性 mock ——
//
// 用途：M1 在**无供应商 key** 的情况下打通「图 → 网关 → 生成 → 引用校验 → 边界声明」全链路。
// 行为（完全确定，便于断言）：
//  1. 从最后一条 user 消息里解析上下文块 `[label: <label>]`（Python 侧 prompt 的固定格式）；
//  2. 取第一个块的标签做引用，输出该块正文的前若干字；
//  3. 若该块来自「行业参考」区块 → 触发边界声明路径；
//     并（MOCK_OPERATIONAL=1，默认开）**故意**输出操作性措辞，用于验证后处理拦截确实生效。
//
// ⚠️ 它不产生真实答案，也不能用于评估回答质量（M1 滞留清单已记录）。
type MockProvider struct {
	EnvOperational bool
}

func NewMockProvider(operational bool) *MockProvider { return &MockProvider{EnvOperational: operational} }

func (p *MockProvider) Name() string { return "mock" }

var (
	labelRe   = regexp.MustCompile(`\[label:\s*([^\]]+)\]`)
	textRe    = regexp.MustCompile(`text:\s*([^\n]+)`)
	industryS = "【行业参考】"
	supportS  = "【本平台能力】"
)

func (p *MockProvider) compose(req ChatRequest) string {
	user := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			user = m.Content
		}
	}

	label := ""
	text := ""
	if m := labelRe.FindStringSubmatch(user); m != nil {
		label = strings.TrimSpace(m[1])
	}
	if m := textRe.FindStringSubmatch(user); m != nil {
		text = strings.TrimSpace(m[1])
	}
	if label == "" {
		return "抱歉，我暂时没有找到相关信息。"
	}
	snippet := []rune(text)
	if len(snippet) > 60 {
		snippet = snippet[:60]
	}

	industry := strings.Contains(user, industryS)
	var answer string
	if industry {
		answer = "按铁路客运的一般做法：" + string(snippet)
	} else {
		answer = "根据平台规则：" + string(snippet)
	}
	answer += " [[" + label + "]]"

	if industry && p.EnvOperational {
		// 故意制造"把行业规则讲成本平台能力"的违规，用来验证 §5.9 ④ 的拦截
		answer += " 您可以点击页面上的入口自助办理。"
	}
	return answer
}

func (p *MockProvider) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	out := p.compose(req)
	return &ChatResponse{Content: out, Provider: p.Name(), TotalTokens: utf8.RuneCountInString(out) + 50}, nil
}

func (p *MockProvider) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
	out := p.compose(req)
	runes := []rune(out)
	for i := 0; i < len(runes); i += 12 {
		end := i + 12
		if end > len(runes) {
			end = len(runes)
		}
		if err := onDelta(string(runes[i:end])); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(4 * time.Millisecond):
		}
	}
	return &ChatResponse{Content: out, Provider: p.Name(), TotalTokens: utf8.RuneCountInString(out) + 50}, nil
}

// New 按配置选择 provider（provider 为空或键缺失时回退 mock，并让调用方记 degraded.llm）
func New(provider, baseURL, apiKey, model string, mockOperational bool) (Provider, bool) {
	if provider == "openai" && apiKey != "" && baseURL != "" {
		return NewOpenAIProvider(baseURL, apiKey, model), false
	}
	return NewMockProvider(mockOperational), true
}
