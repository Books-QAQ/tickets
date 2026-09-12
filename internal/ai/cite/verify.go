// Package cite 引用校验 + 能力边界声明后处理（纯确定性，与引用校验同一层 —— §5.9 ④）
package cite

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
)

// 引用标记约定：[[source_label]]。用双中括号是因为它几乎不会出现在自然语言里，
// 便于确定性剥除（正则无需处理歧义）。
var citeRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// 边界声明的操作性词表（初始版，19.16；后续按误报漏报迭代）
var operationalWords = []string{"点击", "进入", "页面", "我们的", "本站支持", "我的订单", "入口", "按钮", "在应用里"}

// BoundaryPrefix 边界声明模板（§5.9；由生成层注入，不写进知识库正文）
const BoundaryPrefix = "该功能本平台暂未开放，以下是铁路客运行业的一般做法，供参考；具体以车站与官方渠道为准。"

type VerifyResult struct {
	AnswerCleaned     string
	Dropped           []string // 被剥除的引用标签（引用剥除计数）
	ValidSources      []string
	NeedsBoundary     bool // 来源含非 supported → 必须带边界声明
	BoundaryInjected  bool // 本次是否实际注入了边界声明
	OperationalHits   []string
	BoundaryViolation bool // 命中操作性措辞且来源非 supported → 被拦截（boundary_violation_blocked_total）
}

// Verify 引用校验：
//  1. 提取 [[label]]，保留本轮检索到的标签，其余**剥除**并计入 Dropped；
//  2. 依据来源的 capability 判定是否需要边界声明（§5.9 ③④）；
//  3. 若来源非 supported 且答案含操作性措辞 → 拦截并注入边界声明（计数）。
//
// allowed 只应包含**本轮检索到的 chunk**（禁止引用未检索到的出处）。
func Verify(answer string, allowed map[string]kb.Chunk) VerifyResult {
	res := VerifyResult{AnswerCleaned: answer}

	labels := citeRe.FindAllStringSubmatch(answer, -1)
	valid := map[string]kb.Chunk{}
	for _, m := range labels {
		label := strings.TrimSpace(m[1])
		if c, ok := allowed[label]; ok {
			valid[label] = c
			continue
		}
		if !contains(res.Dropped, label) {
			res.Dropped = append(res.Dropped, label)
		}
	}
	for l := range valid {
		res.ValidSources = append(res.ValidSources, l)
	}
	res.AnswerCleaned = citeRe.ReplaceAllStringFunc(answer, func(s string) string {
		label := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "[["), "]]"))
		if _, ok := allowed[label]; ok {
			return s
		}
		return "" // 剥除无效引用
	})
	res.AnswerCleaned = strings.TrimSpace(res.AnswerCleaned)

	// 来源是否全部为 supported（决定是否需要边界声明）
	for _, c := range valid {
		if c.Capability != kb.CapSupported {
			res.NeedsBoundary = true
			break
		}
	}
	hits := OperationalHits(res.AnswerCleaned)
	res.OperationalHits = hits
	if res.NeedsBoundary && len(hits) > 0 {
		// "拦截并强制改写"：确定性剥掉含操作性措辞的句子（M1 不额外调 LLM 改写）。
		// 只加声明、留着"点击入口"这类句子，用户仍会去找一个不存在的入口 —— 那不算拦住。
		res.BoundaryViolation = true
		res.AnswerCleaned, _ = stripOperationalSentences(res.AnswerCleaned)
	}
	if res.NeedsBoundary {
		res.AnswerCleaned = BoundaryPrefix + "\n\n" + strings.TrimSpace(res.AnswerCleaned)
		res.BoundaryInjected = true
	}
	return res
}

// stripOperationalSentences 按句剥掉含操作性措辞的句子，返回清理后的文本与被剥句子数。
// 确定性、零 LLM：只做"sentence 级剔除"，不做改写。
func stripOperationalSentences(answer string) (string, int) {
	var kept []string
	removed := 0
	var cur []rune
	flush := func() {
		s := string(cur)
		cur = cur[:0]
		if strings.TrimSpace(s) == "" {
			return
		}
		if len(OperationalHits(s)) > 0 {
			removed++
			return
		}
		kept = append(kept, s)
	}
	for _, r := range answer {
		cur = append(cur, r)
		if r == '。' || r == '！' || r == '？' || r == '\n' {
			flush()
		}
	}
	flush()
	return strings.Join(kept, ""), removed
}

// OperationalHits 返回命中的操作性措辞
func OperationalHits(answer string) []string {
	var hits []string
	for _, w := range operationalWords {
		if strings.Contains(answer, w) {
			hits = append(hits, w)
		}
	}
	return hits
}

// StripCitations 去掉引用标记（用于给用户看的纯文本 / 缓存键）
func StripCitations(answer string) string {
	return strings.TrimSpace(citeRe.ReplaceAllString(answer, ""))
}

// FormatSource 组装返回给前端的出处条目
func FormatSource(c kb.Chunk) map[string]string {
	return map[string]string{
		"label":      c.SourceLabel,
		"doc_id":     fmt.Sprintf("%d", c.DocID),
		"capability": string(c.Capability),
		"form":       string(c.Form),
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
