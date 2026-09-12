// Package kb 实现知识库的加载、切块、检索（自研 BM25 + 向量 + RRF）与存储。
// 依据：docs/智能AI客服系统-技术设计文档（V1.0）.md §7、§13.1、ADR-10。
package kb

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Form 书写形态（决定切块与阈值分派，见 §7.2）
type Form string

const (
	FormQA    Form = "qa"    // FAQ 式：一个 "## " = 一个 chunk
	FormProse Form = "prose" // 段落式：按 子标题/条款/句边界 回退切块 + 标题路径前缀
)

// Capability 能力状态（来自能力台账 §7.9；检索过滤与 prompt 分区用，见 §5.9）
type Capability string

const (
	CapSupported Capability = "supported"
	CapRoadmap   Capability = "roadmap"
	CapIndustry  Capability = "industry"
)

// 分类闭集（7 类 + 兜底，见 §5.8）
var Categories = []string{"booking", "order", "refund", "account", "travel", "app", "policy"}

// 分类闭集（含兜底）
var CategoriesWithOther = append(append([]string{}, Categories...), "other")

func ValidCategory(c string) bool {
	for _, x := range CategoriesWithOther {
		if c == x {
			return true
		}
	}
	return false
}

// Document 一篇知识库文档（frontmatter + 正文）
type Document struct {
	DocKey       string
	Title        string
	Category     string
	DocType      string
	Visibility   string
	Form         Form
	SubScenario  string
	Capability   Capability
	Source       string
	ScopeKind    string
	ScopeRef     string
	ExpireAt     *time.Time
	ContentHash  string
	Body         string
	RelativePath string
}

// Chunk 知识块（文本与元数据 = 真相源；向量不落 MySQL）
type Chunk struct {
	ID             int64
	DocID          int64
	Seq            int
	SourceLabel    string
	Content        string
	HeadingPath    string
	Category       string
	DocType        string
	Form           Form
	SubScenario    string
	Capability     Capability
	Visibility     string
	ScopeKind      string
	ScopeRef       string
	ExpireAt       *time.Time
	EmbeddingModel string
	ContentHash    string
}

// CapabilityRow 能力台账一行
type CapabilityRow struct {
	SubScenario     string
	Category        string
	Status          Capability
	SystemEntry     string
	Owner           string
	TargetMilestone string
	Source          string
	Note            string
}

// Hash 计算内容指纹（sha256 hex），用于增量入库与幂等 upsert
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NormalizeForm 把 frontmatter 里的形态字符串规范化
func NormalizeForm(s string) (Form, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "qa", "faq", "":
		return FormQA, nil
	case "prose", "paragraph", "section":
		return FormProse, nil
	}
	return "", fmt.Errorf("未知的 form: %q（只允许 qa|prose）", s)
}

// NormalizeCapability 规范化能力状态（缺省 industry：宁可加边界声明，不可漏）
func NormalizeCapability(s string) (Capability, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "supported":
		return CapSupported, nil
	case "roadmap":
		return CapRoadmap, nil
	case "industry", "":
		return CapIndustry, nil
	}
	return "", fmt.Errorf("未知的 capability: %q（只允许 supported|roadmap|industry）", s)
}

// DefaultScopeKind 规范化实例化范围
func DefaultScopeKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "station", "ticket_type", "route":
		return s
	default:
		return "general"
	}
}
