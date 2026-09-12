package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ParseFrontMatter 解析 "---" 包裹的 YAML 子集（扁平 key: value；值可加引号；支持 # 注释）
func ParseFrontMatter(raw []byte) (map[string]string, string, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, text, fmt.Errorf("缺少 frontmatter（文件须以 --- 开头）")
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, text, fmt.Errorf("frontmatter 未闭合（缺少结束的 ---）")
	}
	head := rest[:idx]
	body := strings.TrimPrefix(rest[idx+4:], "\n")

	out := map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, text, fmt.Errorf("frontmatter 行无法解析: %q", line)
		}
		out[strings.TrimSpace(k)] = cleanScalar(v)
	}
	return out, body, nil
}

// cleanScalar 去掉引号与行内注释
func cleanScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ParseCapabilityYAML 解析 kb/capability.yaml（顶层为列表，每项是扁平 map）。
// 只收集**列表项之后**出现的键值：文件头部的 version/updated_at 之类的标量不算条目。
func ParseCapabilityYAML(raw []byte) ([]CapabilityRow, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	var rows []CapabilityRow
	cur := map[string]string{}
	inList := false
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		row := CapabilityRow{
			SubScenario:     cur["sub_scenario"],
			Category:        cur["category"],
			SystemEntry:     cur["system_entry"],
			Owner:           cur["owner"],
			TargetMilestone: cur["target"],
			Source:          cur["source"],
			Note:            cur["note"],
		}
		if row.SubScenario == "" {
			return fmt.Errorf("能力台账存在缺少 sub_scenario 的条目: %v", cur)
		}
		switch cur["status"] {
		case "supported":
			row.Status = CapSupported
		case "roadmap":
			row.Status = CapRoadmap
		case "industry":
			row.Status = CapIndustry
		default:
			return fmt.Errorf("能力台账 %s 的 status 非法: %q", row.SubScenario, cur["status"])
		}
		rows = append(rows, row)
		cur = map[string]string{}
		return nil
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			if err := flush(); err != nil {
				return nil, err
			}
			inList = true
			trimmed = strings.TrimPrefix(trimmed, "- ")
		} else if !inList {
			continue // 列表之前的文件头（version/updated_at 等）不参与条目
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		cur[strings.TrimSpace(k)] = cleanScalar(v)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SubScenario < rows[j].SubScenario })
	return rows, nil
}

// LoadDir 遍历知识库目录：解析 + 严格校验（依据 §7.2 / §7.9 / §5.9）
func LoadDir(root string) ([]Document, []CapabilityRow, error) {
	caps, err := loadCapabilities(filepath.Join(root, "capability.yaml"))
	if err != nil {
		return nil, nil, err
	}
	capIndex := map[string]CapabilityRow{}
	for _, c := range caps {
		capIndex[c.SubScenario] = c
	}

	var docs []Document
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasSuffix(base, ".md") || strings.EqualFold(base, "README.md") || strings.HasPrefix(base, "_") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		doc, err := buildDocument(filepath.ToSlash(rel), raw, capIndex)
		if err != nil {
			return err
		}
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].DocKey < docs[j].DocKey })
	return docs, caps, nil
}

func loadCapabilities(path string) ([]CapabilityRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取能力台账失败（%s）: %w", path, err)
	}
	return ParseCapabilityYAML(raw)
}

func buildDocument(rel string, raw []byte, caps map[string]CapabilityRow) (Document, error) {
	fm, body, err := ParseFrontMatter(raw)
	if err != nil {
		return Document{}, fmt.Errorf("%s: %w", rel, err)
	}
	if strings.TrimSpace(body) == "" {
		return Document{}, fmt.Errorf("%s: 正文为空", rel)
	}
	doc := Document{RelativePath: rel, Body: body, ContentHash: Hash(body)}

	doc.DocKey = strings.TrimSpace(fm["id"])
	if doc.DocKey == "" {
		return Document{}, fmt.Errorf("%s: frontmatter 缺少 id", rel)
	}
	doc.Title = strings.TrimSpace(fm["title"])
	if doc.Title == "" {
		return Document{}, fmt.Errorf("%s: frontmatter 缺少 title", rel)
	}
	doc.Category = strings.TrimSpace(fm["category"])
	if !ValidCategory(doc.Category) || doc.Category == "other" {
		return Document{}, fmt.Errorf("%s: category 非法或越界: %q（只允许 7 类）", rel, doc.Category)
	}
	doc.DocType = strings.TrimSpace(fm["type"])
	if doc.DocType == "" {
		doc.DocType = "faq"
	}
	doc.Visibility = strings.ToLower(strings.TrimSpace(fm["visibility"]))
	if doc.Visibility == "" {
		doc.Visibility = "public"
	}
	if doc.Visibility != "public" && doc.Visibility != "authenticated" {
		return Document{}, fmt.Errorf("%s: visibility 非法: %q", rel, doc.Visibility)
	}
	if doc.Form, err = NormalizeForm(fm["form"]); err != nil {
		return Document{}, fmt.Errorf("%s: %w", rel, err)
	}
	if doc.Capability, err = NormalizeCapability(fm["capability"]); err != nil {
		return Document{}, fmt.Errorf("%s: %w", rel, err)
	}
	doc.ScopeKind = DefaultScopeKind(fm["scope_kind"])
	doc.ScopeRef = strings.TrimSpace(fm["scope_ref"])
	if doc.ScopeKind != "general" && doc.ScopeRef == "" {
		return Document{}, fmt.Errorf("%s: scope_kind=%s 时必须填 scope_ref", rel, doc.ScopeKind)
	}
	doc.SubScenario = strings.TrimSpace(fm["sub_scenario"])
	doc.Source = strings.TrimSpace(fm["source"])
	if exp := strings.TrimSpace(fm["expire_at"]); exp != "" {
		ts, err := time.Parse("2006-01-02", exp)
		if err != nil {
			return Document{}, fmt.Errorf("%s: expire_at 须为 2006-01-02 格式: %q", rel, exp)
		}
		doc.ExpireAt = &ts
	}

	// —— 与能力台账的一致性（§5.9：说不出入口就不算支持）——
	if doc.SubScenario != "" {
		row, ok := caps[doc.SubScenario]
		if !ok {
			return Document{}, fmt.Errorf("%s: sub_scenario %q 不在能力台账中", rel, doc.SubScenario)
		}
		if row.Category != doc.Category {
			return Document{}, fmt.Errorf("%s: category(%s) 与台账(%s) 不一致", rel, doc.Category, row.Category)
		}
	}
	if doc.Capability == CapSupported {
		row, ok := caps[doc.SubScenario]
		if !ok || row.Status != CapSupported || strings.TrimSpace(row.SystemEntry) == "" {
			return Document{}, fmt.Errorf("%s: 标了 capability=supported，但台账里该子场景不是 supported 或缺少 system_entry（说不出入口就不算支持）", rel)
		}
	}
	if doc.Capability == CapRoadmap {
		row, ok := caps[doc.SubScenario]
		if !ok || row.Status != CapRoadmap || strings.TrimSpace(row.TargetMilestone) == "" {
			return Document{}, fmt.Errorf("%s: 标了 capability=roadmap，但台账缺少 target 或状态不是 roadmap", rel)
		}
	}
	if doc.Capability != CapSupported && strings.TrimSpace(doc.Source) == "" {
		return Document{}, fmt.Errorf("%s: 行业/规划类文档必须填 source（权威来源，10⁵ 级要可审计）", rel)
	}
	return doc, nil
}
