package tools

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ---------- 提槽（正则优先，§8.1） ----------

func TestExtractOrderNoAndTicketID(t *testing.T) {
	q := "帮我看下订单 3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b 扣了多少钱"
	if got := ExtractOrderNo(q); got != "3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b" {
		t.Errorf("订单号抽取错误: %q", got)
	}
	if got := ExtractTicketID("车票 12 的手续费"); got != 12 {
		t.Errorf("车票号抽取错误: %d", got)
	}
	// 反例：金额、座位号不能被当成车票号
	if got := ExtractTicketID("退了能拿回 120 元吗"); got != 0 {
		t.Errorf("不该把金额当车票号: %d", got)
	}
	if got := ExtractTicketID("12 号座"); got != 0 {
		t.Errorf("不该把座位号当车票号: %d", got)
	}
}

func TestExtractDate(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 30, 0, 0, time.Local)

	d, raw, ok := ExtractDate("明天有没有票", now)
	if !ok || d.Format("2006-01-02") != "2026-09-13" || raw != "明天" {
		t.Errorf("相对日期错误: %v %q %v", d, raw, ok)
	}
	d, _, ok = ExtractDate("后天从北京走", now)
	if !ok || d.Format("2006-01-02") != "2026-09-14" {
		t.Errorf("后天推算错误: %v", d)
	}
	d, _, ok = ExtractDate("2026-09-20 还有票吗", now)
	if !ok || d.Format("2006-01-02") != "2026-09-20" {
		t.Errorf("绝对日期错误: %v", d)
	}
	d, _, ok = ExtractDate("9月25日有车吗", now)
	if !ok || d.Format("2006-01-02") != "2026-09-25" {
		t.Errorf("月日日期错误: %v", d)
	}
	// 已过日期且没写年份 → 理解为下一年（12 月问 1 月）
	dec := time.Date(2026, 12, 20, 0, 0, 0, 0, time.Local)
	d, _, ok = ExtractDate("1月5日有票吗", dec)
	if !ok || d.Format("2006-01-02") != "2027-01-05" {
		t.Errorf("跨年推算错误: %v", d)
	}
	if _, _, ok := ExtractDate("有没有票", now); ok {
		t.Error("没写日期时不应命中")
	}
}

func TestStationIndexLongestFirstAndAmbiguity(t *testing.T) {
	idx := NewStationIndex(
		[]TerminalRow{{ID: 1, Name: "北京西站", CityID: 1}, {ID: 2, Name: "北京南站", CityID: 1},
			{ID: 3, Name: "上海虹桥站", CityID: 2}},
		[]CityRow{{ID: 1, Name: "北京"}, {ID: 2, Name: "上海"}},
	)

	// 最长优先：不能把"北京西站"拆成"北京"
	hits := idx.MatchStation("明天从北京西站到上海虹桥站有票吗")
	if len(hits) != 2 {
		t.Fatalf("应命中 2 个站名，得到 %d: %+v", len(hits), hits)
	}
	if hits[0].Text != "北京西站" || hits[1].Text != "上海虹桥站" {
		t.Errorf("站名命中错误: %q, %q", hits[0].Text, hits[1].Text)
	}
	if hits[0].TerminalIDs[0] != 1 || hits[1].TerminalIDs[0] != 3 {
		t.Errorf("站点 id 错误: %v %v", hits[0].TerminalIDs, hits[1].TerminalIDs)
	}

	// 歧义：只说"北京" → 对应两个站
	h2 := idx.MatchStation("北京有票吗")
	if len(h2) != 1 || len(h2[0].TerminalIDs) != 2 {
		t.Fatalf("应识别出城市级歧义: %+v", h2)
	}
}

func TestBusAvailabilitySlotsMissingAndAmbiguous(t *testing.T) {
	idx := NewStationIndex(
		[]TerminalRow{{ID: 1, Name: "北京西站", CityID: 1}, {ID: 2, Name: "北京南站", CityID: 1}},
		[]CityRow{{ID: 1, Name: "北京"}},
	)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	tool := busAvailabilityTool{base{Deps{Stations: func() *StationIndex { return idx }, Now: func() time.Time { return now }}}}

	s := tool.Slots("明天有票吗")
	if len(s.Missing) != 2 { // 缺出发站 + 到达站
		t.Errorf("应缺 2 个槽，得到 %v", s.Missing)
	}
	s = tool.Slots("明天从北京到上海有票吗")
	if len(s.Ambiguous) == 0 {
		t.Errorf("“北京”应产生歧义候选，得到 %+v", s)
	}

	// 执行端：槽位不全 → slots_incomplete（**不是** unavailable）
	res := tool.Exec(context.Background(), Identity{}, tool.Slots("明天有票吗"))
	if res.Kind != KindSlotsIncomplete {
		t.Errorf("槽位不全应为 slots_incomplete，得到 %s", res.Kind)
	}
}

// ---------- 脱敏 ----------

func TestMask(t *testing.T) {
	if got := MaskOrderNo("3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b"); got != "3f2a****5a6b" {
		t.Errorf("订单号脱敏错误: %q", got)
	}
	if got := MaskName("张群书"); got != "张*书" {
		t.Errorf("姓名脱敏错误: %q", got)
	}
	if got := MaskName("李四"); got != "李*" {
		t.Errorf("两字姓名脱敏错误: %q", got)
	}
	if got := MaskPhone("13812345678"); got != "138****5678" {
		t.Errorf("手机号脱敏错误: %q", got)
	}
}

// ---------- 熔断 ----------

func TestBreakerOpenHalfOpen(t *testing.T) {
	cnt := &counters{}
	b := newBreaker(3, time.Minute, cnt)
	now := time.Now()
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		b.fail("t1")
	}
	if !b.allow("t1") {
		t.Fatal("未到阈值不该打开")
	}
	b.fail("t1") // 第 3 次 → 打开
	if b.allow("t1") {
		t.Fatal("到达阈值后应拒绝")
	}
	if cnt.get("tool_circuit_rejected_total") == 0 || cnt.get("tool_circuit_open_total") == 0 {
		t.Error("熔断打开与拒绝都要计数（否则工具挂了会伪装成没人问）")
	}

	// 冷却结束 → 半开放行
	now = now.Add(2 * time.Minute)
	if !b.allow("t1") {
		t.Fatal("冷却结束后应放行试探请求")
	}
	if cnt.get("tool_circuit_halfopen_total") == 0 {
		t.Error("半开要计数")
	}
	// 试探成功 → 清零
	b.success("t1")
	if !b.allow("t1") {
		t.Error("成功后应恢复")
	}
}

type counters struct{ m map[string]int64 }

func (c *counters) Inc(k string) {
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[k]++
}

func (c *counters) get(k string) int64 { return c.m[k] }

// ---------- 路由优先级 ----------

func TestRegistryRoutePriorityAndWhitelist(t *testing.T) {
	cnt := &counters{}
	r := BuildRegistry(Deps{Cfg: DefaultConfig(), Cnt: cnt})

	// "订单号" 应命中 order_detail
	cands := r.Route("帮我查订单 3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b", "")
	if len(cands) == 0 || cands[0].Name != "order_detail" {
		t.Fatalf("应命中 order_detail，得到 %+v", cands)
	}
	// 槽位随候选返回（跨语言往返）
	if cands[0].Slots.OrderNo == "" {
		t.Error("候选里应带上抽好的槽位")
	}

	// 同分破平：同时命中 order_detail 与 refund_fee 时，用分类定胜负
	tieQ := "退票手续费怎么算，订单号 3f2a1b4c-5d6e-7f80-9a0b-1c2d3e4f5a6b"
	tie := r.Route(tieQ, "")
	if len(tie) < 2 || tie[0].Score != tie[1].Score {
		t.Fatalf("该问题应产生同分候选，得到 %+v", tie)
	}
	if tie[0].Preferred {
		t.Error("无分类信息时不该擅自标记首选（应交给编排层反问）")
	}
	withCat := r.Route(tieQ, "refund")
	if withCat[0].Name != "refund_fee" || !withCat[0].Preferred {
		t.Errorf("分类 refund 应破平到 refund_fee，得到 %+v", withCat[0])
	}
	if got := r.Route(tieQ, "order")[0].Name; got != "order_detail" {
		t.Errorf("分类 order 应破平到 order_detail，得到 %s", got)
	}

	// 建单工具 Match 恒 0：不能由"用户问了什么"触发
	for _, q := range []string{"投诉", "转人工", "我要人工客服"} {
		for _, c := range r.Route(q, "") {
			if c.Name == "create_support_ticket" {
				t.Errorf("%q 不该路由到建单工具（它只由转人工路径按名调用）", q)
			}
		}
	}

	// 白名单：不存在的工具名必须被拒
	if r.Has("drop_table") {
		t.Error("白名单校验失效")
	}
	if res := r.RunNamed(context.Background(), "drop_table", Identity{UserID: 1}, SlotSet{}); res.Kind != KindUnavailable {
		t.Errorf("未注册工具应返回 unavailable，得到 %s", res.Kind)
	}
}

// ---------- 访客拦截（不碰数据库就该返回） ----------

func TestGuestBlockedBeforeDB(t *testing.T) {
	r := BuildRegistry(Deps{Cfg: DefaultConfig(), Cnt: &counters{}})
	for _, name := range []string{"my_tickets", "order_detail", "unpaid_orders", "refund_progress", "refund_fee"} {
		res := r.RunNamed(context.Background(), name, Identity{Guest: true, GuestKey: "dev-1"}, SlotSet{})
		if res.Kind != KindGuestRequired {
			t.Errorf("%s 游客调用应返回 guest_required，得到 %s", name, res.Kind)
		}
	}
}

// ---------- 退票费确定性计算：19.2 语义确认用的样例对照表 ----------

// TestRefundFeeSampleTable 是本项目 19.2 的核心交付物：
// **把"文档假定的语义"变成一张可逐行核对的样例表**，业务确认这张表就等于确认了严重性最高的金额口径。
// 当前假定语义：距发车 hours_before 小时（含）以前退票，扣 percent%；取满足条件中**最紧**的一档。
func TestRefundFeeSampleTable(t *testing.T) {
	departure := time.Date(2026, 9, 15, 8, 0, 0, 0, time.Local)
	rules := []TierRule{
		{HoursBefore: 24, Percent: 10},
		{HoursBefore: 2, Percent: 30},
		{HoursBefore: 0.5, Percent: 50},
	}

	cases := []struct {
		hoursBefore float64
		amount      int32
		wantPercent int
		wantFeeYuan string
		wantRefund  string
		note        string
	}{
		{72, 120, 10, "12.00", "108.00", "提前 3 天，走最宽档"},
		{24, 120, 10, "12.00", "108.00", "恰好等于阈值（含）→ 仍走 10%"},
		{23.9, 120, 30, "36.00", "84.00", "差一点到 24h → 落到 30% 档"},
		{5, 137, 30, "41.10", "95.90", "金额非整十，验证分位精度"},
		{2, 120, 30, "36.00", "84.00", "恰好等于 2h（含）→ 30%"},
		{1.9, 120, 50, "60.00", "60.00", "低于 2h → 50% 档"},
		{0.5, 120, 50, "60.00", "60.00", "恰好等于 0.5h（含）→ 50%"},
		{0.4, 120, 0, "", "", "低于最低档 → 无档位匹配，不下结论（转人工）"},
		{48, 1, 10, "0.10", "0.90", "1 元票：手续费 0.1 元，分位不丢"},
		{48, 999, 10, "99.90", "899.10", "大额票"},
		{25, 250, 10, "25.00", "225.00", "整百"},
		{-3, 120, 0, "", "", "已过发车（误车）→ 在 Exec 里走 blocked，不进计算"},
	}

	fmt.Println("距发车(小时) | 票面(元) | 档位 | 手续费(元) | 实退(元) | 说明")
	fmt.Println("-------------|---------|------|-----------|---------|-----")
	for _, c := range cases {
		now := departure.Add(-time.Duration(c.hoursBefore * float64(time.Hour)))
		res := ComputeRefundFee(c.amount, departure, now, rules)
		if c.wantPercent == 0 {
			if !res.NoTierMatched && res.MatchedRule == nil {
				fmt.Printf("%12.2f | %7d |  -   |     -     |    -    | %s\n", c.hoursBefore, c.amount, c.note)
				continue
			}
			// 负小时（已过发车）也会落到"无档位"
			if res.MatchedRule == nil {
				fmt.Printf("%12.2f | %7d |  -   |     -     |    -    | %s\n", c.hoursBefore, c.amount, c.note)
				continue
			}
			t.Errorf("%.2fh 不该匹配到档位（得到 %.0fh/%d%%）", c.hoursBefore, res.MatchedRule.HoursBefore, res.Percent)
			continue
		}
		gotFee, gotRefund := Yuan(res.FeeCents), Yuan(res.RefundCents)
		fmt.Printf("%12.2f | %7d | %3d%% | %9s | %7s | %s\n", c.hoursBefore, c.amount, res.Percent, gotFee, gotRefund, c.note)
		if res.Percent != c.wantPercent || gotFee != c.wantFeeYuan || gotRefund != c.wantRefund {
			t.Errorf("%.2fh/%d元: 期望 %d%%/%s/%s，得到 %d%%/%s/%s",
				c.hoursBefore, c.amount, c.wantPercent, c.wantFeeYuan, c.wantRefund,
				res.Percent, gotFee, gotRefund)
		}
	}
}

// 闸门关闭时 refund_fee 不下结论（宁可转人工不猜金额）
func TestRefundFeeGatedByDefault(t *testing.T) {
	r := BuildRegistry(Deps{Cfg: DefaultConfig(), Cnt: &counters{}})
	res := r.RunNamed(context.Background(), "refund_fee", Identity{UserID: 7}, SlotSet{})
	if res.Kind != KindUnavailable || res.Reason != "penalty_semantics_unconfirmed" {
		t.Fatalf("默认应因 19.2 闸门关闭而拒绝下结论，得到 %s/%s", res.Kind, res.Reason)
	}
	if res.PathHint != "transfer_tool_unavailable" {
		t.Errorf("应给出转人工路径提示，得到 %q", res.PathHint)
	}
}
