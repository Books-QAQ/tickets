package kb

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Tokenize 中文 bigram + ASCII 词 分词。
//
// 为什么不用 text.split()：中文整句会被当成 1 个 token，BM25 直接报废（见 §7.4）。
// 规则：
//   - 连续汉字段：长度 1 → 该字；长度 >=2 → 相邻二字 bigram（"退票费" → 退票/票费）
//   - 连续 ASCII 字母数字：小写化成一个词（长度 >=2 保留；单字符丢弃）
//   - 其余（标点/空白）作分隔
func Tokenize(s string) []string {
	out := make([]string, 0, len(s))
	var han []rune
	var ascii []rune

	flushHan := func() {
		if len(han) == 1 {
			out = append(out, string(han[0]))
		} else {
			for i := 0; i+1 < len(han); i++ {
				out = append(out, string(han[i:i+2]))
			}
		}
		han = han[:0]
	}
	flushASCII := func() {
		if len(ascii) >= 2 {
			out = append(out, strings.ToLower(string(ascii)))
		}
		ascii = ascii[:0]
	}

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flushASCII()
			han = append(han, r)
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushHan()
			ascii = append(ascii, r)
		default:
			flushHan()
			flushASCII()
		}
	}
	flushHan()
	flushASCII()
	return out
}

// TokenizeUnique 去重后的词项（写查询与算 df 时用）
func TokenizeUnique(s string) []string {
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, t := range Tokenize(s) {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ExtractScopeRef 从问题里抽实例化维度（站点/线路），用于两级检索的第一级过滤（§7.1）。
// M1 只做基于知识库已有 scope_ref 词表的确定性匹配（零 LLM）。
// 返回命中的 scope_kind 与 scope_ref；未命中返回空串。
func ExtractScopeRef(question string, known map[string][]string) (kind, ref string) {
	// known: scope_kind -> 候选标识列表（来自 kb_chunks 去重）
	// 按标识长度降序匹配，避免 "北京" 抢先于 "北京西站"
	type cand struct {
		kind string
		ref  string
	}
	var cands []cand
	for k, refs := range known {
		for _, r := range refs {
			if r == "" {
				continue
			}
			cands = append(cands, cand{kind: k, ref: r})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		return len([]rune(cands[i].ref)) > len([]rune(cands[j].ref))
	})
	for _, c := range cands {
		if strings.Contains(question, c.ref) {
			return c.kind, c.ref
		}
	}
	return "", ""
}

var _ = fmt.Sprintf
