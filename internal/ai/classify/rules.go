// Package classify 规则层分类与前置意图（Go 侧确定性打分，零 LLM）。
// 依据：§5.8（7 类闭集、前置意图优先）、§5.3（E1~E4）
package classify

import (
	"sort"
	"strings"

	"github.com/Books-QAQ/tickets/internal/ai/kb"
)

// 强信号权重 3 / 模糊词 1（§5.8 分类链路①）
const (
	weightStrong = 3
	weightWeak   = 1
)

// SufficientGap 规则层置信度阈值：top1-top2 间距不足则交给 LLM 仲裁（E4）
const SufficientGap = 3

type Result struct {
	PreIntent string         // complaint | human | greet | none
	Scores    map[string]int // 分类名 -> 得分（软路由要用，禁止丢弃）
	Top1      string
	Top2      string
	Gap       int
	Matched   map[string][]string
}

// PreIntent 前置意图（确定性词表，零 LLM）：命中即不进分类（E1/E2）
//
// 例外规则（M1 实测补充）：**信息类提问不算投诉行为**。
// "投诉渠道是什么/投诉电话多少/怎么投诉" 是在问信息（应该走检索 → policy 类），
// 而 "我要投诉你们/举报" 才该直接转人工。只靠"投诉"二字会把两者一起吞掉。
func PreIntent(question string) string {
	q := normalize(question)
	for _, kw := range complaintWords {
		if strings.Contains(q, kw) {
			if isInformationalComplaint(q) {
				break // 在问"投诉渠道/怎么投诉"这类信息 → 交给分类走检索
			}
			return "complaint"
		}
	}
	for _, kw := range humanWords {
		if strings.Contains(q, kw) {
			return "human"
		}
	}
	for _, kw := range greetWords {
		if strings.Contains(q, kw) {
			return "greet"
		}
	}
	return "none"
}

// isInformationalComplaint 判断"提到投诉但其实在问信息"
func isInformationalComplaint(q string) bool {
	if !strings.Contains(q, "投诉") {
		return false
	}
	for _, kw := range informationalWords {
		if strings.Contains(q, kw) {
			return true
		}
	}
	return false
}

// Score 规则打分：返回各类得分、top1/top2 与间距
func Score(question string) Result {
	q := normalize(question)
	scores := map[string]int{}
	matched := map[string][]string{}
	for cat, words := range keywordTable {
		for _, kw := range words {
			if strings.Contains(q, kw.text) {
				scores[cat] += kw.weight
				matched[cat] = append(matched[cat], kw.text)
			}
		}
	}
	res := Result{PreIntent: PreIntent(question), Scores: scores, Matched: matched}
	type kv struct {
		cat string
		sc  int
	}
	var list []kv
	for cat, sc := range scores {
		if sc > 0 {
			list = append(list, kv{cat, sc})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].sc == list[j].sc {
			return list[i].cat < list[j].cat
		}
		return list[i].sc > list[j].sc
	})
	if len(list) > 0 {
		res.Top1 = list[0].cat
	}
	if len(list) > 1 {
		res.Top2 = list[1].cat
		res.Gap = list[0].sc - list[1].sc
	} else if len(list) == 1 {
		res.Gap = list[0].sc
	}
	return res
}

// Enough 规则层是否足够自信（E3 的 "gap 充足"）
func (r Result) Enough() bool {
	if r.Top1 == "" {
		return false
	}
	return r.Gap >= SufficientGap
}

type keyword struct {
	text   string
	weight int
}

// keywordTable 7 类闭集的关键词表（M1 初始版；扩充需重跑 held-out 评测，见 §15）
var keywordTable = map[string][]keyword{
	"booking": {
		{"购票", weightStrong}, {"买票", weightStrong}, {"订票", weightStrong}, {"预订", weightStrong},
		{"余票", weightStrong}, {"票价", weightStrong}, {"班次", weightStrong}, {"车次", weightWeak},
		{"开售", weightStrong}, {"什么时候开卖", weightStrong}, {"选座", weightStrong}, {"座位", weightWeak},
		{"候补", weightStrong}, {"联程", weightStrong}, {"接续", weightStrong}, {"分段买", weightStrong},
		{"学生票", weightStrong}, {"儿童票", weightStrong}, {"团体票", weightStrong}, {"限售", weightWeak},
		{"怎么买", weightWeak}, {"能买几张", weightWeak},
	},
	"order": {
		{"订单", weightStrong}, {"支付", weightStrong}, {"付款", weightStrong}, {"扣款", weightStrong},
		{"付不了", weightStrong}, {"未支付", weightStrong}, {"待支付", weightStrong}, {"发票", weightStrong},
		{"报销凭证", weightStrong}, {"开票", weightStrong}, {"短信通知", weightStrong}, {"行程提示", weightStrong},
		{"重复扣", weightStrong}, {"支付失败", weightStrong}, {"没收到短信", weightStrong},
	},
	"refund": {
		{"退票", weightStrong}, {"退钱", weightStrong}, {"退款", weightStrong}, {"手续费", weightStrong},
		{"改签", weightStrong}, {"变更到站", weightStrong}, {"退改", weightStrong}, {"到账", weightWeak},
		{"退不了", weightStrong}, {"停运", weightStrong}, {"晚点", weightStrong}, {"退多少钱", weightStrong},
		{"扣多少", weightStrong},
	},
	"account": {
		{"注册", weightStrong}, {"登录", weightStrong}, {"密码", weightStrong}, {"找回密码", weightStrong},
		{"手机号", weightStrong}, {"邮箱", weightStrong}, {"实名", weightStrong}, {"身份核验", weightStrong},
		{"证件", weightStrong}, {"常用联系人", weightStrong}, {"账号", weightStrong}, {"被锁", weightStrong},
		{"风控", weightWeak}, {"登录不上", weightStrong},
	},
	"travel": {
		{"检票", weightStrong}, {"取票", weightStrong}, {"进站", weightStrong}, {"出站", weightStrong},
		{"行李", weightStrong}, {"带多少", weightWeak}, {"宠物", weightStrong}, {"违禁品", weightStrong},
		{"老人", weightWeak}, {"重点旅客", weightStrong}, {"遗失", weightStrong}, {"丢了", weightStrong},
		{"落车上了", weightStrong}, {"中转", weightStrong}, {"换乘", weightStrong}, {"候车", weightWeak},
		{"电子客票", weightStrong}, {"提前多久", weightWeak},
	},
	"app": {
		{"打不开", weightStrong}, {"报错", weightStrong}, {"闪退", weightStrong}, {"白屏", weightStrong},
		{"卡住", weightStrong}, {"验证码", weightStrong}, {"收不到", weightWeak}, {"推送", weightStrong},
		{"缓存", weightWeak}, {"浏览器", weightWeak}, {"加载", weightWeak}, {"按钮", weightWeak},
		{"app", weightWeak}, {"App", weightWeak},
	},
	"policy": {
		{"投诉渠道", weightStrong}, {"活动", weightStrong}, {"积分", weightStrong}, {"会员", weightStrong},
		{"政策", weightStrong}, {"公告", weightStrong}, {"优惠", weightStrong}, {"规则在哪", weightWeak},
	},
}

var complaintWords = []string{"投诉", "举报", "曝光", "差评", "315", "消协", "起诉", "告你们", "曝光你们"}

// informationalWords 信息类措辞：与"投诉"同时出现时，判为"在问信息"而非"要投诉"（M1 实测补充）
var informationalWords = []string{"渠道", "电话", "方式", "在哪", "哪里", "怎么", "如何", "流程", "规则", "多久", "入口"}

var humanWords = []string{"转人工", "人工客服", "真人", "找客服", "叫人", "人工服务", "客服电话"}

var greetWords = []string{"你好", "您好", "在吗", "在么", "hi", "hello", "嗨", "有人吗"}

func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.ReplaceAll(s, "\u3000", "")
	return s
}

// 编译期断言：分类名必须落在知识库的 7 类闭集内（防止表里写错分类名）
var _ = func() bool {
	for cat := range keywordTable {
		if !kb.ValidCategory(cat) || cat == "other" {
			panic("classify: 关键词表出现闭集外的分类: " + cat)
		}
	}
	return true
}()
