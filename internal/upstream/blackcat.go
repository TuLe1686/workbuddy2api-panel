// blackcat.go 夜猫子任务（black_cat）+ 新手礼包/补偿 API。
//
// 判据（WorkBuddy-Daily 项目实测口径 + 本网关验证）：black_cat 要求在
// **23:00–08:00（本地时区）窗口内**完成 3 次 glm-5.2 对话并上报 chat 事件链；
// 窗口外行为不计分。真实对话走网关既有 ChatStream（glm-5.2），事件链用
// ReportChatActivityModel（chat_5 同款上报形状）。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// InNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（需要再完成几次对话）。
// 任务不存在返回 0（无可做）；拉取失败返回错误。
func (c *Client) BlackcatNeed(a *auth.Auth) (int64, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode == "black_cat" {
			if t.Claimed || t.Current >= t.Target {
				return 0, nil
			}
			return t.Target - t.Current, nil
		}
	}
	return 0, nil
}

// nightPrompts 夜猫子对话文案池（2026-09-28 治本改造）。
//
// 背景：此前每号每晚发一字不差的 "1+1等于几？直接回答。"——大量账号凌晨
// 1~5 点从同一 IP 用完全相同的裸文本调 glm-5.2，上游内容审核按"同文本批量
// 请求"命中，403/11140（displayMsg: 内容未通过安全审核）一夜 219 次；错误
// 分类又把 11140 当封号，误禁了 32 个健康账号（凭证/余额实测完好）。
//
// 文案设计原则：
//   - 生活化、口语化、无敏感词——真人深夜会问的东西；
//   - 每条都足够短（black_cat 只要求完成对话，token 消耗可忽略）；
//   - 16 条，(账号, 日期, 次数) 散列选文：同号同晚的 3 次对话互不相同，
//     不同账号天然错开，且跨日期轮换（同一号隔几天才会重复同文案）。
var nightPrompts = []string{
	"明天香港天气怎么样？",
	"推荐一道简单的家常菜，今晚想自己做饭。",
	"帮我列一下明天早上出门要带的五样东西。",
	"最近睡眠不太好，有什么改善的小技巧吗？",
	"用一句话解释一下什么是复利。",
	"我养的多肉叶子发黄了，可能是什么原因？",
	"推荐一部适合睡前看的轻松电影。",
	"明天要早起赶高铁，几点睡觉比较合适？",
	"帮我想三个明天午饭吃什么的选择。",
	"简单说一下跑步前热身要做些什么。",
	"给一句适合发朋友圈的早安文案。",
	"我想开始学做咖啡，从哪种开始比较好？",
	"深圳和广州周末去哪个玩更好？各说一个理由。",
	"一件衣服打七折后是 210 元，原价是多少？",
	"推荐一个十分钟的居家拉伸动作。",
	"用大白话解释一下什么是通货膨胀。",
}

// pickNightPrompt 为一次夜间对话选文案：(uid, 日期, nth) FNV-1a 散列取模。
// 确定性：同号同晚同一 nth 恒取同一条（可测试），跨 nth/日期/账号自然散开。
func pickNightPrompt(uid, date string, nth int) string {
	h := uint32(2166136261)
	key := uid + "|" + date + "|" + fmt.Sprint(nth)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return nightPrompts[h%uint32(len(nightPrompts))]
}

// RunNightChats 夜猫子：发 need 次 glm-5.2 真实对话（读干流）并上报事件链。
// 返回成功次数。
//
// 出站形态（2026-09-28 治本改造，对齐正常用户 chat 流量）：
//   - 走与用户 chat 相同的 prepareBody 出站链（指纹脱敏 / effort 归一 /
//     prompt_cache_key 注入）——裸 body 是上游审核的明显异常形态；
//   - 带 system 消息（真实客户端形态，同正常链路的 ensureConsoleSystem 哲学）；
//   - 文案从 nightPrompts 按 (uid, 日期, nth) 散列选取，杜绝同文本批量请求。
func (c *Client) RunNightChats(a *auth.Auth, need int) (int64, error) {
	var ok int64
	date := time.Now().Format("2006-01-02")
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model": "glm-5.2",
			"messages": []map[string]any{
				{"role": "system", "content": "你是我的日常小助手，回答简短自然就好。"},
				{"role": "user", "content": pickNightPrompt(a.UID, date, i)},
			},
			"stream": true,
		})
		// 与 ChatStreamContext 内部同一出站链：指纹脱敏/effort 归一/缓存键注入。
		prepared := c.prepareBody(body, a.Realm(), a.UID, "")
		rc, status, respBody, err := c.ChatStream(a, prepared, "", ChatMeta{})
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("第 %d 次对话失败: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := c.ReportChatActivityModel(a, fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		time.Sleep(4 * time.Second)
	}
	return ok, nil
}

// ClaimGift 领取新手礼包（每号一次，已领返回业务错误）。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed 检查昨日是否漏签（heatmap cell score==0）。
func (c *Client) HeatmapYesterdayMissed(a *auth.Auth) (bool, error) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}
