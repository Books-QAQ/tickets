package kb

import (
	"strings"
	"testing"
)

// 两形态切块（§7.2）：qa 按 "## " 切且不加前缀；prose 按子标题切且必须带标题路径前缀
func TestChunkTwoForms(t *testing.T) {
	qa := Document{
		DocKey: "order-x", Category: "order", DocType: "faq", Form: FormQA,
		Capability: CapSupported, Visibility: "public", ScopeKind: "general",
		Body: "## 问题一\n\n问：怎么退\n问：怎么退票\n\n答：在订单页退。\n\n## 问题二\n\n问：多久到账\n问：什么时候到\n\n答：一到三天。\n",
	}
	chunks, err := ChunkDocument(qa)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("qa 应切出 2 块，实际 %d", len(chunks))
	}
	if chunks[0].HeadingPath != "" {
		t.Errorf("qa 不应有标题路径前缀，得到 %q", chunks[0].HeadingPath)
	}
	if !strings.Contains(chunks[0].SourceLabel, "order-x·段落1") {
		t.Errorf("qa 出处标签格式不对: %s", chunks[0].SourceLabel)
	}
	if chunks[0].ContentHash == "" {
		t.Error("content_hash 不能为空（增量入库依赖它）")
	}

	prose := Document{
		DocKey: "station-x", Category: "travel", DocType: "policy", Form: FormProse,
		Capability: CapSupported, Visibility: "public", ScopeKind: "station", ScopeRef: "北京西站",
		Title: "北京西站规则",
		Body:  "## 检票时间\n\n一、本站实行电子客票，凭有效身份证件进站。\n二、开车前 25 分钟开始检票。\n三、开车前 5 分钟停止检票。\n",
	}
	pchunks, err := ChunkDocument(prose)
	if err != nil {
		t.Fatal(err)
	}
	if len(pchunks) == 0 {
		t.Fatal("prose 应切出至少 1 块")
	}
	first := pchunks[0]
	if first.HeadingPath != "检票时间" {
		t.Errorf("prose 标题路径应为「检票时间」，得到 %q", first.HeadingPath)
	}
	if !strings.HasPrefix(first.Content, "【检票时间】") {
		t.Errorf("prose 块必须以标题路径前缀开头（对抗语义稀释），得到 %q", first.Content[:min(20, len(first.Content))])
	}
	if first.ScopeRef != "北京西站" {
		t.Errorf("实例化维度必须带到 chunk: %q", first.ScopeRef)
	}
}

// 中文 bigram 分词（§7.6：中文整句当 1 个 token 会让 BM25 报废）
func TestTokenizeChineseBigram(t *testing.T) {
	toks := Tokenize("退票手续费")
	want := []string{"退票", "票手", "手续", "续费"}
	if len(toks) != len(want) {
		t.Fatalf("期望 %d 个 bigram，得到 %d: %v", len(want), len(toks), toks)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("第 %d 个 token 应为 %s，得到 %s", i, want[i], toks[i])
		}
	}
	// 单字与 ASCII 混排
	mixed := Tokenize("401 报错")
	if len(mixed) < 2 {
		t.Errorf("混排分词结果过少: %v", mixed)
	}
}

// BM25：真倒排打分与过滤谓词（分类/可见性过滤必须在打分前生效）
func TestBM25SearchAndFilter(t *testing.T) {
	ix := NewIndex()
	ix.Add(1, "退票手续费按时间档位收取", ChunkMeta{ChunkID: 1, Category: "refund", Visibility: "public", Capability: CapSupported})
	ix.Add(2, "页面报错 401 表示未登录", ChunkMeta{ChunkID: 2, Category: "app", Visibility: "public", Capability: CapSupported})
	ix.Add(3, "内部规则：退票手续费内部口径", ChunkMeta{ChunkID: 3, Category: "refund", Visibility: "authenticated", Capability: CapSupported})
	ix.Finalize()

	hits := ix.Search("退票手续费怎么算", 10, nil)
	if len(hits) == 0 {
		t.Fatal("应有命中")
	}
	if hits[0].ChunkID != 1 && hits[0].ChunkID != 3 {
		t.Errorf("首位应是含「退票手续费」的块，得到 %d", hits[0].ChunkID)
	}

	// 游客可见性过滤：authenticated 的块不能出现在结果里（安全边界）
	guest := Filter{Visibility: []string{"public"}}
	for _, h := range ix.Search("退票手续费", 10, guest.Allow) {
		if h.ChunkID == 3 {
			t.Error("游客检索命中了 authenticated 块 —— 过滤是安全边界，必须拦住")
		}
	}
	// 分类过滤
	refundOnly := Filter{Category: "refund", Visibility: []string{"public"}}
	for _, h := range ix.Search("退票手续费 页面报错", 10, refundOnly.Allow) {
		if h.ChunkID == 2 {
			t.Error("category 过滤失效：命中了 app 类块")
		}
	}
}

// RRF 融合（§7.4：score = Σ 1/(k+rank)，k=60）
func TestRRFFusion(t *testing.T) {
	a := []Hit{{ChunkID: 1, Score: 9}, {ChunkID: 2, Score: 5}}
	b := []Hit{{ChunkID: 2, Score: 0.9}, {ChunkID: 3, Score: 0.8}}
	fused := Fuse(a, b)
	if len(fused) != 3 {
		t.Fatalf("融合后应有 3 个候选，得到 %d", len(fused))
	}
	// 2 在两路都靠前 → 应排第一
	if fused[0].ChunkID != 2 {
		t.Errorf("两路共现的块应排第一，得到 %d", fused[0].ChunkID)
	}
	want := 1.0/float64(RRFK+2) + 1.0/float64(RRFK+1)
	if diff := fused[0].Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("RRF 分数应为 %.6f，得到 %.6f", want, fused[0].Score)
	}
}

// 台账解析：文件头的 version/updated_at 不算条目
func TestParseCapabilityYAML(t *testing.T) {
	raw := []byte(`# 注释
version: 1
updated_at: 2026-09-12
items:
  - sub_scenario: order_query
    category: order
    status: supported
    system_entry: "GET /orders/:orderNo"
  - sub_scenario: waitlist
    category: booking
    status: roadmap
    target: "M5-3"
    source: "铁路旅客运输规程"
`)
	rows, err := ParseCapabilityYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应解析出 2 条，得到 %d（文件头被当成条目了？）", len(rows))
	}
	if rows[0].SubScenario != "order_query" || rows[0].Status != CapSupported {
		t.Errorf("第一条解析错误: %+v", rows[0])
	}
	if rows[1].Status != CapRoadmap || rows[1].TargetMilestone != "M5-3" {
		t.Errorf("第二条解析错误: %+v", rows[1])
	}
}

// frontmatter 解析与取值清洗
func TestParseFrontMatter(t *testing.T) {
	raw := []byte("---\nid: kb-x\ntitle: \"标题: 带冒号\"\nform: qa\n---\n\n正文内容\n")
	fm, body, err := ParseFrontMatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fm["id"] != "kb-x" {
		t.Errorf("id 解析错误: %q", fm["id"])
	}
	if fm["title"] != "标题: 带冒号" {
		t.Errorf("带引号且含冒号的值应完整保留: %q", fm["title"])
	}
	if strings.TrimSpace(body) != "正文内容" {
		t.Errorf("正文解析错误: %q", body)
	}
}

// 形态规范化：未知 form 必须报错（不能默认成 qa 混进库）
func TestNormalizeForm(t *testing.T) {
	if f, _ := NormalizeForm(""); f != FormQA {
		t.Errorf("空 form 应默认 qa，得到 %s", f)
	}
	if _, err := NormalizeForm("paragraph2"); err == nil {
		t.Error("未知 form 应当报错")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
