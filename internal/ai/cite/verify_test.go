package cite

import (
	"strings"
	"testing"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
)

func chunk(label string, cap kb.Capability) kb.Chunk {
	return kb.Chunk{SourceLabel: label, Category: "refund", Form: kb.FormQA, Capability: cap}
}

// 引用校验：只放行本轮检索到的出处，其余剥除并计数
func TestVerifyDropsUnknownCitations(t *testing.T) {
	allowed := map[string]kb.Chunk{"kb/a#1": chunk("kb/a#1", kb.CapSupported)}
	answer := "按规则收费 [[kb/a#1]]，另外见 [[kb/不存在#9]]。"

	res := Verify(answer, allowed)
	if res.Dropped == nil || len(res.Dropped) != 1 || res.Dropped[0] != "kb/不存在#9" {
		t.Fatalf("应剥除 1 条无效引用，得到 %v", res.Dropped)
	}
	if strings.Contains(res.AnswerCleaned, "kb/不存在#9") {
		t.Error("无效引用必须从答案里剥掉")
	}
	if !strings.Contains(res.AnswerCleaned, "[[kb/a#1]]") {
		t.Error("有效引用应保留")
	}
	if res.NeedsBoundary {
		t.Error("来源是 supported，不该加边界声明")
	}
}

// 边界声明：来源含非 supported → 必须注入声明，且剥掉操作性句子（§5.9 ④）
func TestBoundaryGuardBlocksOperational(t *testing.T) {
	allowed := map[string]kb.Chunk{"kb/w#1": chunk("kb/w#1", kb.CapRoadmap)}
	answer := "候补的一般做法是先预付票款排队。 [[kb/w#1]] 您可以点击页面上的入口自助办理。"

	res := Verify(answer, allowed)
	if !res.NeedsBoundary || !res.BoundaryInjected {
		t.Fatal("来源是 roadmap，必须注入边界声明")
	}
	if !res.BoundaryViolation {
		t.Error("出现操作性措辞且来源非 supported，应标记为违规拦截")
	}
	if !strings.HasPrefix(res.AnswerCleaned, BoundaryPrefix) {
		t.Error("边界声明必须在答案最前面")
	}
	if strings.Contains(res.AnswerCleaned, "点击") || strings.Contains(res.AnswerCleaned, "入口") {
		t.Errorf("操作性句子必须被剥掉，实际答案：%s", res.AnswerCleaned)
	}
	if !strings.Contains(res.AnswerCleaned, "先预付票款排队") {
		t.Error("非操作性内容不应被误删")
	}
}

// 平台自有能力的答案里出现"点击"是允许的（不该误伤）
func TestOperationalAllowedForSupported(t *testing.T) {
	allowed := map[string]kb.Chunk{"kb/o#1": chunk("kb/o#1", kb.CapSupported)}
	answer := "您可以点击「我的车票」里的退票按钮办理。 [[kb/o#1]]"
	res := Verify(answer, allowed)
	if res.BoundaryViolation || res.BoundaryInjected {
		t.Error("supported 来源不应触发边界拦截")
	}
	if !strings.Contains(res.AnswerCleaned, "点击") {
		t.Error("supported 来源的操作性措辞不应被剥掉")
	}
}

// 无有效出处（E15）：全部引用都被剥除
func TestVerifyNoValidSource(t *testing.T) {
	res := Verify("随便说一段 [[kb/x#1]]", map[string]kb.Chunk{})
	if len(res.ValidSources) != 0 {
		t.Errorf("没有有效出处时应为空，得到 %v", res.ValidSources)
	}
	if len(res.Dropped) != 1 {
		t.Errorf("应记录 1 条剥除，得到 %v", res.Dropped)
	}
}
