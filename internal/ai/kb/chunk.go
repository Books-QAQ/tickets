package kb

import (
	"fmt"
	"regexp"
	"strings"
)

// 切块参数（§7.2）：qa 不重叠；prose 单块上限 600 rune、相邻块保留 1 句重叠
const (
	DefaultMaxChunkRunes = 600
	OverlapMaxRunes      = 120 // 参与重叠的句子最长长度（超过则不重叠，避免块被一句撑大）
)

// clausePrefix 条款起始标记：一、/（一）/(1)/1./第N条 —— prose 的第二层回退边界
var clausePrefix = regexp.MustCompile(`^\s*(?:[一二三四五六七八九十]+[、.．]|（[一二三四五六七八九十]+）|\([0-9]+\)|[0-9]+[、.．]|第[一二三四五六七八九十百零]+条)`)

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

// ChunkDocument 按书写形态切块（qa / prose），见 §7.2
func ChunkDocument(doc Document) ([]Chunk, error) {
	switch doc.Form {
	case FormQA:
		return chunkQA(doc), nil
	case FormProse:
		return chunkProse(doc, DefaultMaxChunkRunes), nil
	default:
		return nil, fmt.Errorf("文档 %s 的 form 非法: %q", doc.DocKey, doc.Form)
	}
}

// chunkQA：一个 "## " 段落 = 一个知识点 = 一个 chunk；无 "## " 时整篇作为一个块
func chunkQA(doc Document) []Chunk {
	body := strings.ReplaceAll(doc.Body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	var blocks []string
	var cur []string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") && !strings.HasPrefix(ln, "### ") {
			if s := strings.TrimSpace(strings.Join(cur, "\n")); s != "" {
				blocks = append(blocks, s)
			}
			cur = []string{ln}
			continue
		}
		cur = append(cur, ln)
	}
	if s := strings.TrimSpace(strings.Join(cur, "\n")); s != "" {
		blocks = append(blocks, s)
	}
	if len(blocks) == 0 {
		blocks = []string{strings.TrimSpace(body)}
	}

	out := make([]Chunk, 0, len(blocks))
	for i, b := range blocks {
		if strings.TrimSpace(b) == "" {
			continue
		}
		out = append(out, chunkFor(doc, i+1, b, fmt.Sprintf("%s·段落%d", doc.DocKey, i+1), ""))
	}
	return out
}

// chunkProse：三层回退 子标题 → 条款编号 → 句子边界；块前拼标题路径前缀（对抗语义稀释）
func chunkProse(doc Document, maxRunes int) []Chunk {
	body := strings.ReplaceAll(doc.Body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	type section struct {
		path  string // "A > B"
		title string // 最近一级标题
		text  string
	}
	var sections []section
	var stack []string // 标题栈（最多保留 3 级用于路径）
	curPath, curTitle := strings.Join(stack, " > "), doc.Title
	var curLines []string

	flush := func() {
		if strings.TrimSpace(strings.Join(curLines, "\n")) != "" {
			sections = append(sections, section{path: curPath, title: curTitle, text: strings.Join(curLines, "\n")})
		}
		curLines = nil
	}

	for _, ln := range lines {
		if m := headingRe.FindStringSubmatch(ln); m != nil {
			flush()
			level := len(m[1])
			title := strings.TrimSpace(m[2])
			for len(stack) >= level {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, title)
			if len(stack) > 3 { // 路径最多 3 级（lint 同口径）
				stack = stack[len(stack)-3:]
			}
			curPath = strings.Join(stack, " > ")
			curTitle = title
			continue
		}
		curLines = append(curLines, ln)
	}
	flush()

	var out []Chunk
	seq := 0
	for _, sec := range sections {
		units := splitUnits(sec.text)
		for gi, group := range packUnits(units, maxRunes) {
			seq++
			content := strings.Join(group, "\n")
			heading := sec.path
			if heading == "" {
				heading = doc.Title
			}
			// 标题路径前缀：块内自带上下文（相似度不再被长段落摊薄）
			prefixed := "【" + heading + "】\n" + content
			label := fmt.Sprintf("%s·%s#%d", doc.DocKey, sec.title, gi+1)
			if strings.TrimSpace(content) == "" {
				continue
			}
			out = append(out, chunkFor(doc, seq, prefixed, label, heading))
		}
	}
	return out
}

func chunkFor(doc Document, seq int, content, label, headingPath string) Chunk {
	return Chunk{
		Seq:         seq,
		SourceLabel: label,
		Content:     content,
		HeadingPath: headingPath,
		Category:    doc.Category,
		DocType:     doc.DocType,
		Form:        doc.Form,
		SubScenario: doc.SubScenario,
		Capability:  doc.Capability,
		Visibility:  doc.Visibility,
		ScopeKind:   doc.ScopeKind,
		ScopeRef:    doc.ScopeRef,
		ExpireAt:    doc.ExpireAt,
		ContentHash: Hash(content),
	}
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '!', '?', ';', '\n':
		return true
	}
	return false
}

// splitUnits 句子/条款级切分（prose 的第三层回退）
func splitUnits(text string) []string {
	var units []string
	var cur []rune
	flush := func() {
		if s := strings.TrimSpace(string(cur)); s != "" {
			units = append(units, s)
		}
		cur = cur[:0]
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if clausePrefix.MatchString(trimmed) {
			flush()
		}
		for _, r := range line {
			cur = append(cur, r)
			if isSentenceEnd(r) {
				flush()
			}
		}
		flush()
	}
	flush()
	return units
}

// packUnits 把句子组装成不超过 maxRunes 的块，块间保留 1 句重叠（防条款被切断）
func packUnits(units []string, maxRunes int) [][]string {
	var out [][]string
	var cur []string
	curLen := 0
	for _, u := range units {
		n := len([]rune(u))
		if curLen > 0 && curLen+n > maxRunes {
			out = append(out, cur)
			last := cur[len(cur)-1]
			if len([]rune(last)) <= OverlapMaxRunes {
				cur = []string{last}
				curLen = len([]rune(last))
			} else {
				cur = nil
				curLen = 0
			}
		}
		cur = append(cur, u)
		curLen += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}
