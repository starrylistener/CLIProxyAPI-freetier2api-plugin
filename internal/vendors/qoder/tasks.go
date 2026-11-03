package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件承载 Qoder 的账号运营类可选能力：额度查询与签到。

// 本文件实现 Qoder 的额度查询。
//
// 与 WorkBuddy 的积分余额不同，Qoder 的额度是「用量桶」：上游返回
// userQuota（套餐）、addOnQuota（加购）与 orgResourcePackage（组织共享
// 资源包）三个对象，各有 used / total(或 cap) / remaining。
// 归一成宿主的 QuotaFetchResponse（Summary 放关键数字，Groups 放明细）。

// quotaHTTPTimeout 是额度相关请求的上限。
const quotaHTTPTimeout = 30 * time.Second

// QuotaBucket 是一个额度桶。
type QuotaBucket struct {
	Used      float64
	Total     float64
	Remaining float64
	ResetTime string
}

// Quota 是一次额度查询结果。
type Quota struct {
	Plan            string
	UserQuota       *QuotaBucket
	AddonQuota      *QuotaBucket
	OrgPackage      *QuotaBucket
	IsQuotaExceeded bool
	ExpiresAt       int64
}

// FetchQuota 查询账号额度。
func FetchQuota(ctx context.Context, cred *Credential) (*Quota, error) {
	endpoints, errEndpoints := resolveEndpoints(cred.Region, cred.VPCEndpoint)
	if errEndpoints != nil {
		return nil, errEndpoints
	}
	bearer, errToken := quotaBearerToken(ctx, cred)
	if errToken != nil {
		return nil, errToken
	}
	raw, errQuota := httpGetBearerJSON(ctx, endpoints.QuotaEndpoint, bearer)
	if errQuota != nil {
		return nil, errQuota
	}
	return &Quota{
		Plan:            fetchPlanTierName(ctx, endpoints, bearer),
		IsQuotaExceeded: raw["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(raw, "expiresAt")),
		UserQuota:       extractQuotaBucket(raw, "userQuota"),
		AddonQuota:      extractQuotaBucket(raw, "addOnQuota"),
		OrgPackage:      extractOrgPackage(raw),
	}, nil
}

// knownQuotaBucketKeys 是已经单独解析过的顶层桶，通用扫描要跳过。
var knownQuotaBucketKeys = []string{"userQuota", "addOnQuota", "addonQuota", "user_quota", "add_on_quota", "orgResourcePackage"}

// extractOrgPackage 读取「资源包」额度（企业/团队账号的主额度桶）。
//
// 企业账号的可用额度不在 userQuota / addOnQuota 里：前者是订阅席位
// （3000/3000 用尽），后者可能整包过期（total=3000、remaining=0），真正能用的
// 在另一个顶层桶里（网页端叫「资源包」，issue #70 的截图是 0/2000）。
//
// **不写死键名**：个人账号上取不到这个字段的样本，键名与内部字段名都没有
// 实测证据（个人账号的响应只有 userQuota / addOnQuota，字段是
// `{total, remaining, used, unit, percentage}`）。所以这里改成结构发现：
// 凡是顶层里「同时有总额与剩余两个数值」又不属于已知桶的对象，就按资源包读，
// 内部字段兼容 total/cap/capacity 与 remaining/available/left。
// 这样上游改键名也不会再漏读，也不会因为猜错名字而静默返回 nil。
func extractOrgPackage(data map[string]any) *QuotaBucket {
	// 1. 已知键名优先：国内版团队账号的「资源包」叫 orgResourcePackage，
	// 总额字段叫 cap，另带 available 说明当前能不能取用。available=false
	// （过期 / 停用 / 已退还）时余额取不出来，按桶不存在处理（issue #70）。
	if obj, okObj := data["orgResourcePackage"].(map[string]any); okObj {
		if available, okAvailable := obj["available"].(bool); okAvailable && !available {
			return nil
		}
		if len(obj) == 0 {
			return nil
		}
		numbers := readBucketNumbers(obj)
		bucket := &QuotaBucket{Used: numbers.used, Total: numbers.total, Remaining: numbers.remaining}
		bucket.ResetTime = resetTimeOf(obj)
		return bucket
	}
	// 2. 结构发现兜底：其他账号类型若键名不叫 orgResourcePackage，凡是顶层里
	// 「同时有总额与剩余两个数值」又不属于已知桶的对象，就按资源包读，
	// 内部字段兼容 total/cap/capacity 与 remaining/available/left。
	var best *QuotaBucket
	for key, value := range data {
		if slices.Contains(knownQuotaBucketKeys, key) {
			continue
		}
		object, okObject := value.(map[string]any)
		if !okObject {
			continue
		}
		bucket := quotaBucketFromObject(object)
		// 多个候选时取余额最大的那个：页头要显示的是「还能用多少」。
		if bucket == nil || (best != nil && bucket.Remaining <= best.Remaining) {
			continue
		}
		best = bucket
	}
	return best
}

// quotaBucketFromObject 按已知别名把一个桶对象读成 QuotaBucket。
// 总额与剩余必须**同时**存在，否则返回 nil——避免把别的对象误认成额度桶。
func quotaBucketFromObject(object map[string]any) *QuotaBucket {
	numbers := readBucketNumbers(object)
	if !numbers.hasTotal || !numbers.hasRemaining {
		return nil
	}
	bucket := &QuotaBucket{Used: numbers.used, Total: numbers.total, Remaining: numbers.remaining}
	bucket.ResetTime = resetTimeOf(object)
	return bucket
}

// resetTimeOf 读桶的重置时刻，兼容 resetTime 与 reset_time 两种拼写。
//
// 上游不同接口的字段拼写不一致。两处解析各写一套迟早会漂：漏掉别名的那条
// 路径会把「有重置时间」显示成「不重置」（#71 第二轮复审 P2-7）。
func resetTimeOf(object map[string]any) string {
	if reset, okReset := object["resetTime"].(string); okReset {
		return reset
	}
	if reset, okReset := object["reset_time"].(string); okReset {
		return reset
	}
	return ""
}

// bucketNumbers 是桶内三个数值及其「是否存在」标记。
type bucketNumbers struct {
	total, remaining, used          float64
	hasTotal, hasRemaining, hasUsed bool
}

// readBucketNumbers 按同一套别名取桶内数值。
//
// 资源包（结构发现，键名不固定）和固定键桶（userQuota / addOnQuota）走同一份
// 别名表：两套各写一份时，上游把 total 改名成 cap，只会有一边认得出来，另一
// 边静默变 0。缺 used 时按 total - remaining 反推，仅当两者都在。
func readBucketNumbers(object map[string]any) bucketNumbers {
	numbers := bucketNumbers{}
	numbers.total, numbers.hasTotal = firstNumberField(object, "total", "cap", "capacity")
	numbers.remaining, numbers.hasRemaining = firstNumberField(object, "remaining", "available", "left")
	numbers.used, numbers.hasUsed = firstNumberField(object, "used")
	if !numbers.hasUsed && numbers.hasTotal && numbers.hasRemaining {
		numbers.used = numbers.total - numbers.remaining
		if numbers.used < 0 {
			numbers.used = 0
		}
	}
	return numbers
}

// toFloat 把 JSON 里的数值字段读成 float64（兼容多种数字形态）。
func toFloat(m map[string]any, key string) float64 {
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, _ := value.Float64()
		return parsed
	}
	return 0
}

// firstNumberField 按顺序取第一个存在且为数值的字段。
func firstNumberField(object map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, okValue := object[key]
		if !okValue {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return typed, true
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case json.Number:
			parsed, errParsed := typed.Float64()
			if errParsed == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// quotaBearerToken 把凭证换成额度接口认识的 bearer token。
//
// 设备令牌（dt-…）可直接用；PAT（pt-…）必须先经 jobToken 交换拿到
// securityOauthToken——上游额度接口不认 PAT 本身。
func quotaBearerToken(ctx context.Context, cred *Credential) (string, error) {
	endpoints, errEndpoints := resolveEndpoints(cred.Region, cred.VPCEndpoint)
	if errEndpoints != nil {
		return "", errEndpoints
	}
	deviceToken, _ := ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		return deviceToken, nil
	}
	seed := FingerprintSeed("", cred.Token)
	jobToken, errExchange := ExchangeJobToken(ctx, cred.Token,
		DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return "", classifyCredentialError(fmt.Errorf("exchange token: %w", errExchange))
	}
	oauthToken := StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return "", NewError("qoder_credential_invalid", "jobToken 响应里没有 securityOauthToken", http.StatusUnauthorized)
	}
	return oauthToken, nil
}

// httpGetBearerJSON 发一个带 Bearer 的 GET 并解析成 JSON 对象。
func httpGetBearerJSON(ctx context.Context, endpoint, token string) (map[string]any, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, NewError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, errDo := hostbridge.Client(ctx, quotaHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, NewError("qoder_upstream_transient", errDo.Error(), http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, NewError("qoder_upstream_error", errRead.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return nil, NewError("qoder_upstream_error",
			fmt.Sprintf("额度接口返回 HTTP %d: %s", resp.StatusCode, core.Truncate(string(body), 300)), status)
	}
	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return nil, NewError("qoder_upstream_error", "额度接口返回的不是 JSON: "+errUnmarshal.Error(), http.StatusBadGateway)
	}
	return result, nil
}

// fetchPlanTierName 查询套餐名；失败不影响额度展示，只留空。
func fetchPlanTierName(ctx context.Context, endpoints Endpoints, token string) string {
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	if name, okName := result["plan_tier_name"].(string); okName {
		return name
	}
	return ""
}

// extractQuotaBucket 提取额度桶。
//
// 「桶不存在」的判据是**空对象**（上游对没有的桶返回 `{}`），不是「值全零」：
// Free 计划的 userQuota 就是 `{used:0, total:0, remaining:0}`，按全零吞掉会让
// 页面显示「上游未返回额度」，而真实语义是「有额度信息，只是余额为 0」。
func extractQuotaBucket(data map[string]any, key string) *QuotaBucket {
	obj, okObj := data[key].(map[string]any)
	if !okObj {
		return nil
	}
	// 桶对象存在即视为上游承认这份额度，缺字段按 0 处理（不走 quotaBucketFromObject
	// 的「两字段齐全才算」判据——那是给键名不固定的资源包做误认防护的）。
	numbers := readBucketNumbers(obj)
	bucket := &QuotaBucket{
		Used:      numbers.used,
		Total:     numbers.total,
		Remaining: numbers.remaining,
	}
	bucket.ResetTime = resetTimeOf(obj)
	return bucket
}

// extractOrgPackage 提取组织共享资源包（orgResourcePackage）。
//
// 国内版团队账号的「资源包」就是它：由组织统一购买、团队成员共享，字段
// 与套餐桶不同——总额叫 cap，另外带 available 说明当前能不能取用。
// available=false（过期 / 停用 / 已退还）时余额取不出来，按桶不存在处理，
// 否则账号表会显示一份无法使用的额度。
// BuildQuotaResponse 把上游额度翻译成宿主的归一化结构。
func BuildQuotaResponse(quota *Quota) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}
	if quota.Plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{
			Plan: quota.Plan, TierName: quota.Plan, TierID: quota.Plan,
		}
	}
	// 全 0 的桶既不进 Summary 也不进 Groups：Free 账号的「套餐额度 0/0」
	// 会在面板上占掉一整排 chip 加一条 0% 进度条，纯粹是噪音。
	// 但若**所有**桶都是空的（额度真用光了），至少保留一个，否则面板只会
	// 显示「暂无额度数据」，看不出「已经用完」。
	anyData := quotaBucketHasData(quota.UserQuota) || quotaBucketHasData(quota.AddonQuota) || quotaBucketHasData(quota.OrgPackage)
	keepEmpty := func(bucket *QuotaBucket) bool { return !anyData || quotaBucketHasData(bucket) }
	// 三桶按「谁还有余额」排序后输出：插件的 auth.Totals 取**第一个有容量的
	// 族**当页头数字（total>0 即算有容量），所以顺序就是语义。
	// 排错的表现：企业账号的页头永远停在已过期那桶的「0 / 3000」。
	//
	// Groups（面板上的额度条）与 Summary 必须同序：页头数字取的是 Summary 里
	// 第一个有容量的族，两条路径各排各的就会出现「页头 2000/3000、第一条
	// 却写着 0/3000」这种自相矛盾的展示。
	for _, family := range orderedQuotaFamilies(quota) {
		if !keepEmpty(family.bucket) {
			continue
		}
		if group := quotaGroup(family.group, family.bucket); group != nil {
			response.Groups = append(response.Groups, *group)
		}
		// total 必须一并输出：页面的「剩余/总额 + 剩余率」要它才能算比例。
		// 上游对 Free 计划可能给 total=0（额度按剩余量计），此时仍输出 0——
		// 页面会显示 "—"，比编一个数诚实。
		// Unit 一律留空：宿主面板把 unit 直接拼在数字后面，填 "credits" 会渲染成
		// 「497 credits」中英混排，而 Label 里的「额度」已经说明了计量口径。
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: family.prefix + "_remaining", Label: family.label + "剩余额度", Value: family.bucket.Remaining},
			pluginapi.QuotaMetric{Key: family.prefix + "_total", Label: family.label + "总额度", Value: family.bucket.Total},
			pluginapi.QuotaMetric{Key: family.prefix + "_used", Label: family.label + "已用额度", Value: family.bucket.Used})
	}
	if quota.IsQuotaExceeded {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "quota_exceeded", Label: "额度已用尽", Value: 1})
	}
	if expires := quotaExpiryUnixSeconds(quota.ExpiresAt); expires > 0 {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "expires_at", Label: "额度到期（Unix 秒）", Value: float64(expires)})
	}
	return response
}

// quotaExpiryUnixSeconds 把上游的到期时间规整成 Unix 秒；不展示时返回 0。
//
// 上游给的是**毫秒**（253402214400000 = 9999-12-31），直接当秒输出会让面板
// 渲染成「253,402,214,400,000」这种天文数字。另外「永不过期」类时间戳没有
// 信息量，一律折叠成不展示。
func quotaExpiryUnixSeconds(raw int64) int64 {
	if raw <= 0 {
		return 0
	}
	seconds := raw
	if seconds > 1e12 { // 大于 1e12 的秒级时间戳不存在，只可能是毫秒
		seconds /= 1000
	}
	const maxPlausibleUnix = 4102444800 // 2100-01-01，超出视为「永不过期」
	if seconds <= 0 || seconds > maxPlausibleUnix {
		return 0
	}
	return seconds
}

// quotaFamilyMetric 是一个额度桶在 Summary 里的键前缀、chip 上的名字与
// Summary 里用的短名。
type quotaFamilyMetric struct {
	prefix string
	label  string
	group  string
	bucket *QuotaBucket
}

// orderedQuotaFamilies 按「谁还有余额」排定额度桶的输出顺序。
//
// 插件 auth.Totals 取第一个有容量的族当页头数字，而「有容量」包含
// 「total>0、remaining==0」（额度合法用完）。Qoder 的桶会出现这种状态：
//
//	企业账号  套餐 3000/3000 用尽 → 个人拓展包过期(total=3000,remaining=0)
//	          → 组织资源包 2000/3000 ← 真正可用的在这里
//	Free 计划 套餐 0/0（上游就是给 0）→ 个人拓展包 50/200
//
// 若按固定顺序输出，页头会被已过期的桶占住，显示「0 / 3000」，而账号其实
// 还能用 2000。所以按三级排：有剩余 → 有总额 → 其余，同级保持
// 套餐 → 拓展包 → 组织资源包 的自然顺序（个人账号优先看套餐）。
func orderedQuotaFamilies(quota *Quota) []quotaFamilyMetric {
	all := []quotaFamilyMetric{
		{prefix: "user_quota", label: "套餐", group: "套餐额度", bucket: quota.UserQuota},
		{prefix: "addon_quota", label: "个人拓展包", group: "个人拓展包", bucket: quota.AddonQuota},
		{prefix: "org_quota", label: "组织资源包", group: "组织资源包", bucket: quota.OrgPackage},
	}
	ordered := make([]quotaFamilyMetric, 0, len(all))
	taken := make([]bool, len(all))
	appendMatching := func(keep func(quotaFamilyMetric) bool) {
		for index, family := range all {
			if taken[index] || family.bucket == nil || !keep(family) {
				continue
			}
			taken[index] = true
			ordered = append(ordered, family)
		}
	}
	appendMatching(func(family quotaFamilyMetric) bool { return family.bucket.Remaining > 0 })
	appendMatching(func(family quotaFamilyMetric) bool { return family.bucket.Total > 0 })
	appendMatching(func(quotaFamilyMetric) bool { return true })
	return ordered
}

// quotaBucketHasData 判断一个额度桶是否承载真实数据（任一维度非零）。
func quotaBucketHasData(bucket *QuotaBucket) bool {
	if bucket == nil {
		return false
	}
	return bucket.Total != 0 || bucket.Remaining != 0 || bucket.Used != 0
}

// quotaGroup 把额度桶转成「剩余比例」分组。
func quotaGroup(displayName string, bucket *QuotaBucket) *pluginapi.QuotaGroup {
	if bucket == nil {
		return nil
	}
	fraction := 0.0
	switch {
	case bucket.Total > 0:
		fraction = core.ClampFraction(bucket.Remaining / bucket.Total)
	case bucket.Remaining > 0:
		fraction = 1
	}
	description := fmt.Sprintf("已用 %.0f / 总计 %.0f，剩余 %.0f", bucket.Used, bucket.Total, bucket.Remaining)
	return &pluginapi.QuotaGroup{
		DisplayName: displayName,
		Buckets: []pluginapi.QuotaBucket{{
			Window:            "billing-cycle",
			RemainingFraction: fraction,
			ResetTime:         bucket.ResetTime,
			Description:       description,
		}},
	}
}

// 签到状态取值。
const (
	// CheckinClaimed 表示本次成功领取。
	CheckinClaimed = "claimed"
	// CheckinAlreadyClaimed 表示本轮已领取（幂等命中）。
	CheckinAlreadyClaimed = "already_claimed"
	// CheckinNoCampaign 表示没有可领取的活动。
	CheckinNoCampaign = "no_campaign"
	// CheckinWindowNotOpen 表示新一轮的领取窗口尚未开启。
	CheckinWindowNotOpen = "window_not_open"
	// CheckinWindowMissed 表示上一轮窗口已结束且没有领取（官方不补领）。
	CheckinWindowMissed = "window_missed"
	// CheckinNotConfirmed 表示上游接受了领取但未能确认发放。
	CheckinNotConfirmed = "not_confirmed"
	// CheckinBlocked 表示上游以风控去重为由拒绝了发放。
	CheckinBlocked = "blocked"
	// CheckinNoToken 表示凭证里没有可用 token。
	CheckinNoToken = "no_token"
	// CheckinError 表示执行失败。
	CheckinError = "error"
)

// campaignZone 是上游活动窗口使用的时区（UTC+8 固定偏移）。
//
// 用固定偏移而不是 Asia/Shanghai：容器镜像未必带 tzdata，而中国自 1991
// 年起不再使用夏令时，固定 +08:00 与官方口径完全一致。
var campaignZone = time.FixedZone("UTC+8", 8*60*60)

// dailyRoundHour 是上游每日开启新一轮领取的时刻（UTC+8 小时数）。
//
// 官方规则：每日 10:00（UTC+8）开启新一轮，窗口持续到次日 10:00 前；
// 需主动领取，错过不补领。签到判定必须以它为准，而不是本地自然日。
const dailyRoundHour = 10

// checkinHTTPTimeout 是签到相关请求的上限。
const checkinHTTPTimeout = 60 * time.Second

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Status 是结果状态（见上面的常量）。
	Status string
	// Amount 是本次领取的积分数。
	Amount int
	// Message 是可读说明。
	Message string
	// WindowDate 是签到窗口日期（YYYY-MM-DD，UTC+8），用于按窗口幂等。
	//
	// 它取自活动自己的 startAt：上游每天 10:00 换一条活动，因此这个日期
	// 就是「哪一轮」的标识，比本地自然日更贴近上游语义。
	WindowDate string
	// CampaignID 是本次涉及的活动 ID（便于领取后回查发放记录）。
	CampaignID string
	// NotRecorded 报告本次结果不应被记入签到历史。
	//
	// 置位的情形：窗口未开启、窗口已错过、领取被风控拒绝、领取后未能
	// 确认发放。这些情形都没有真正领到积分，记成「今日已签到」会让页面
	// 显示假性成功，而上游窗口错过不补领——积分就此永久丢失。
	NotRecorded bool
	// StreakDays 是连续签到天数（来自只读统计）。
	StreakDays int
	// TotalClaimDays 是累计领取天数。
	TotalClaimDays int
	// TotalRewardCredits 是累计获得积分。
	TotalRewardCredits int
}

// campaignInfo 是活动列表条目。
type campaignInfo struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
	// RequiredAchievementKey 是领取所需先完成的成就键（官方活动页字段）。
	RequiredAchievementKey string `json:"requiredAchievementKey"`
	// AchievementCompleted 报告该成就是否已完成。
	AchievementCompleted bool `json:"achievementCompleted"`
	// UnavailableReason 是上游给出的不可领取原因。
	UnavailableReason string `json:"unavailableReason"`
}

// claimResponse 是领取响应。
type claimResponse struct {
	GrantID  string `json:"grantId"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Benefit  *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
	// FailureCode 是上游拒绝发放时给出的机器可读原因。
	//
	// 参考实现 qoder2api-hub 记录：同一人多账号会被判重，返回
	// SAME_PERSON_ALREADY_CLAIMED（官方按「人」而非「账号」去重）。
	FailureCode string `json:"failureCode"`
	// ErrorCode / ErrorMessage 是上游错误体的另一种形态。
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// rewardResponse 是单个活动的发放状态（只读回查）。
type rewardResponse struct {
	GrantID    string `json:"grantId"`
	Status     string `json:"status"`
	Replayed   bool   `json:"replayed"`
	GrantedAt  string `json:"grantedAt"`
	ClaimedAt  string `json:"claimedAt"`
	CampaignID string `json:"campaignId"`
	Benefit    *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// dailyCheckinStatus 是 legacy 接口的只读统计。
type dailyCheckinStatus struct {
	Status             string `json:"status"`
	CurrentStreakDays  int    `json:"currentStreakDays"`
	TotalClaimDays     int    `json:"totalClaimDays"`
	TotalRewardCredits int    `json:"totalRewardCredits"`
}

// Checkin 执行一次签到。
//
// identity 是用户提供的官方组件设备身份；没有时它为零值，请求里不发机器头
// （见 checkinHeaders）。国内版不需要身份；国际版没有身份就看不到活动，
// 此时结果会说明原因而不是笼统报「无可用签到活动」。
func Checkin(ctx context.Context, cred *Credential, identity MachineIdentity) (*CheckinResult, error) {
	if cred.VPCEndpoint != "" {
		return &CheckinResult{Status: CheckinNoCampaign, NotRecorded: true, Message: "企业专属账号暂不支持签到"}, nil
	}
	result := &CheckinResult{Status: CheckinError}
	bearer := checkinBearer(cred.Token)
	if bearer == "" {
		result.Status = CheckinNoToken
		result.Message = "凭证里没有可用 token"
		return result, nil
	}
	result = campaignsCheckin(ctx, cred.Region, bearer, identity, result)
	readDailyCheckinStats(ctx, cred.Region, bearer, identity, result)
	return result, nil
}

// checkinBearer 取签到用的 bearer。
//
// 只需 device token：上游对签到接口不校验 cosy 签名，用 PAT 反而会失败。
func checkinBearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	deviceToken, _ := ParseOAuthSecret(trimmed)
	if deviceToken != "" {
		return deviceToken
	}
	return trimmed
}

// checkinNow 是签到判定用的时钟。
//
// 抽成包级变量是为了让「窗口未开启」这类时间相关分支可以被测试：签到
// 的正确性几乎全在时刻上（上游每日 10:00 UTC+8 换轮），不注入时钟就
// 只能覆盖当前时刻这一个点。
var checkinNow = func() time.Time { return time.Now().In(campaignZone) }

// campaignsCheckin 是真实发放积分的领取流程。
//
// 判定完全以**上游活动自己的窗口**（startAt/endAt，UTC+8）为准，而不是本地
// 自然日：上游每天 10:00 换一条新活动，9 点那一轮看到的是**上一轮**已领取
// 的活动，把它当成「今日已领取」就会显示假性成功，而当日窗口随后开启时
// 已经错过——官方不补领，积分就此永久丢失。
func campaignsCheckin(ctx context.Context, region Region, bearer string, identity MachineIdentity, result *CheckinResult) *CheckinResult {
	status, body, raw := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/campaigns", bearer, identity, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, core.Truncate(raw, 300))
		result.NotRecorded = true
		return result
	}
	list, okList := body.(map[string]any)
	if !okList {
		result.Message = fmt.Sprintf("活动列表格式异常: %s", core.Truncate(raw, 300))
		result.NotRecorded = true
		return result
	}
	campaigns := parseCampaigns(list["campaigns"])

	now := checkinNow()

	// 找出当前正在进行的活动，以及属于往轮（已领取或已错过）的活动。
	//
	// 判据是**窗口是否覆盖当前时刻**：上游每轮的窗口是
	// [当天 10:00, 次日 10:00)，所以 9 点这一轮覆盖的正是「昨天 10:00 开启、
	// 今天 10:00 关闭」的那一轮。它在 9 点时窗口仍然有效，因此会被归到
	// current —— 这不是缺陷，而是上游的真实语义。
	//
	// 「今日已领」与「上一轮已领」的区分靠 isTodayRoundWindow：只有
	// 今天 10:00 开启的那一轮才算今天领的。
	var current *campaignInfo
	var past *campaignInfo
	// blocked 记录第一条「可领但成就未完成」的活动：没有可领活动时用它给出
	// 可操作的原因，而不是笼统的「无可用签到活动」。
	var blocked *campaignInfo
	for index := range campaigns {
		campaign := &campaigns[index]
		if campaign.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		// 成就未完成的 CLAIMABLE 活动**不能**被当成当前轮去领取：上游会拒绝，
		// 而同一列表里今天已领的每日活动会被它覆盖，导致后续轮次误报失败。
		if campaignAchievementBlocked(campaign) {
			if blocked == nil {
				blocked = campaign
			}
			continue
		}
		if campaign.StartAt <= 0 {
			// 上游偶尔省略时间字段：按当前轮处理。
			if current == nil {
				current = campaign
			}
			continue
		}
		if campaignWindowCovers(campaign, now) {
			if current == nil || campaign.ClaimStatus == "CLAIMABLE" {
				current = campaign
			}
			continue
		}
		if past == nil {
			past = campaign
		}
	}

	// 当前轮活动存在：按其 claimStatus 决定领取还是幂等。
	if current != nil {
		result.WindowDate = campaignWindowDate(current)
		result.CampaignID = current.CampaignID
		switch current.ClaimStatus {
		case "CLAIMABLE":
			// 只领今天 10:00 开启的那一轮。9 点时上游仍可能下发上一轮
			// 的可领活动（那一轮还没领过时），领它等于领错轮次。
			if !isTodayRoundWindow(current, now) {
				result.Status = CheckinWindowNotOpen
				result.NotRecorded = true
				result.Message = fmt.Sprintf("上游领取窗口 %02d:00（UTC+8）开启，当前未开启", dailyRoundHour)
				return result
			}
			return claimCampaign(ctx, region, bearer, identity, current, result)
		case "CLAIMED":
			// 已领取，但要确认领的是**哪一轮**：只有今天 10:00 开启的
			// 那一轮才算「今日已领」。9 点那一轮看到的是昨天 10:00 开启
			// 的窗口——它虽仍覆盖当下，但属于上一轮，今日窗口尚未开启。
			if !isTodayRoundWindow(current, now) {
				result.Status = CheckinWindowNotOpen
				result.NotRecorded = true
				result.Message = fmt.Sprintf("上游领取窗口 %02d:00（UTC+8）开启，当前未开启", dailyRoundHour)
				return result
			}
			result.Status = CheckinAlreadyClaimed
			result.Message = "今日已领取"
			return result
		case "ACHIEVEMENT_NOT_COMPLETED":
			// 官方活动页有 requiredAchievementKey / achievementCompleted 字段：
			// 这类活动要先完成指定成就才能领。
			result.Status = CheckinNoCampaign
			result.NotRecorded = true
			result.Message = achievementMessage(current)
			return result
		default:
			// 其它状态（如上游风控标记）：如实报告，不当成已领。
			result.Status = CheckinNoCampaign
			result.NotRecorded = true
			result.Message = fmt.Sprintf("活动状态 %s，未领取%s", current.ClaimStatus, unavailableSuffix(current))
			return result
		}
	}

	// 没有当前轮活动：区分「今日窗口尚未开启」与「今日窗口已错过未领」。
	if past != nil {
		if past.ClaimStatus == "CLAIMED" {
			// 上一轮已领、当前轮还没下发 —— 这正是 9:00 那一轮看到的形态。
			// 它既不是「今日已领取」，也不是失败：今日窗口尚未开启。
			result.Status = CheckinWindowNotOpen
			result.NotRecorded = true
			result.WindowDate = campaignWindowDate(past)
			result.Message = fmt.Sprintf("上游领取窗口 %02d:00（UTC+8）开启，当前未开启", dailyRoundHour)
			return result
		}
		// 往轮没领成：官方不补领，如实告知免得用户以为漏了操作。
		result.Status = CheckinWindowMissed
		result.NotRecorded = true
		result.WindowDate = campaignWindowDate(past)
		result.Message = fmt.Sprintf("上一轮领取窗口（%s）已结束且未领取，官方不补领", result.WindowDate)
		return result
	}
	if blocked != nil {
		// 有活动，但都被成就门槛挡住：给出「先完成哪个成就」，而不是「无活动」。
		result.Status = CheckinNoCampaign
		result.NotRecorded = true
		result.CampaignID = blocked.CampaignID
		result.Message = achievementMessage(blocked)
		return result
	}
	result.Status = CheckinNoCampaign
	result.NotRecorded = true
	result.Message = noCampaignMessage(region, identity)
	return result
}

// campaignAchievementBlocked 报告活动是否「可领但成就未完成」。
//
// 这类活动在列表里仍是 CLAIMABLE，直接领取会被上游拒绝；选择当前轮时
// 必须跳过，否则会覆盖同一列表里今天已领的每日活动，把「今日已领取」
// 误报成成就未完成。
func campaignAchievementBlocked(campaign *campaignInfo) bool {
	if campaign == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(campaign.ClaimStatus), "CLAIMED") {
		return false
	}
	return strings.TrimSpace(campaign.RequiredAchievementKey) != "" && !campaign.AchievementCompleted
}

// noCampaignMessage 说明「没有可领取活动」的真实原因。
//
// 国际版的活动只下发给登录过官方客户端的那台真机（上游按 machinetoken 精确
// 校验，2026-10-23 实测：派生值、容器内官方组件、改一位的值全部无效）。没带
// 身份时不能笼统地说「无可用签到活动」——那会让用户以为今天本来就没活动。
func noCampaignMessage(region Region, identity MachineIdentity) string {
	if region == RegionGlobal && !identity.Native() {
		return "国际版活动只下发给登录过官方客户端的设备；请在该机器上运行 runtime-info，把输出的 machineToken/machineType 填入凭证或插件配置"
	}
	return "无可用签到活动"
}

// claimCampaign 领取一次活动，并在领取后回读上游确认发放。
//
// 「领取请求返回 200」不等于「积分到账」——上游可能因风控拒绝发放却仍返回
// 成功形态。因此这里在拿到 CLAIMED 后再回查 reward 与额度，两个信号都没
// 落实就不算成功，避免控制台显示假性成功。
func claimCampaign(ctx context.Context, region Region, bearer string, identity MachineIdentity, target *campaignInfo, result *CheckinResult) *CheckinResult {
	// 领取前先读一次额度作为基线：领取后要与它比较，判断积分是否真的到账。
	quotaBefore, errBefore := readAddonQuotaTotal(ctx, region, bearer)
	hadQuotaBefore := errBefore == nil

	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw := doCheckinRequest(ctx, region, http.MethodPost, claimPath, bearer, identity, nil)
	if status != http.StatusOK {
		result.Status = CheckinError
		result.NotRecorded = true
		result.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, core.Truncate(raw, 300))
		return result
	}
	var claim claimResponse
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &claim) != nil {
		result.Status = CheckinError
		result.NotRecorded = true
		result.Message = fmt.Sprintf("领取响应格式异常: %s", core.Truncate(raw, 300))
		return result
	}
	switch strings.ToUpper(claim.Status) {
	case "CLAIMED":
		if claim.Replayed {
			// 上游对重复领取返回 CLAIMED + replayed:true，语义是幂等命中。
			result.Status = CheckinAlreadyClaimed
			result.Message = "今日已领取"
			return result
		}
		return confirmClaim(ctx, region, bearer, identity, target, claim, quotaBefore, hadQuotaBefore, result)
	case "BLOCKED":
		// 官方按「人」去重：同一人多账号时只允许领一份。
		result.Status = CheckinBlocked
		result.NotRecorded = true
		result.Message = fmt.Sprintf("上游以风控去重为由拒绝发放%s", claimFailureSuffix(claim))
		return result
	case "ALREADY_CLAIMED", "REPLAYED":
		result.Status = CheckinAlreadyClaimed
		result.Message = "今日已领取"
		return result
	case "ACHIEVEMENT_NOT_COMPLETED", "ACHIEVEMENT_NOT_COMPLETE", "ACHIEVEMENT_REQUIRED":
		// 领取时上游才校验成就：如实告知需要先完成哪个成就。
		result.Status = CheckinNoCampaign
		result.NotRecorded = true
		result.Message = achievementMessage(target)
		return result
	case "":
		result.Status = CheckinError
		result.NotRecorded = true
		result.Message = fmt.Sprintf("领取响应缺少状态: %s", core.Truncate(raw, 300))
		return result
	default:
		if strings.Contains(strings.ToUpper(claim.FailureCode), "ACHIEVEMENT") ||
			strings.Contains(strings.ToUpper(claim.ErrorCode), "ACHIEVEMENT") {
			result.Status = CheckinNoCampaign
			result.NotRecorded = true
			result.Message = achievementMessage(target)
			return result
		}
		result.Status = CheckinNoCampaign
		result.NotRecorded = true
		result.Message = fmt.Sprintf("上游状态 %s，未领取%s", claim.Status, claimFailureSuffix(claim))
		return result
	}
}

// confirmClaim 回读上游确认积分确实发放。
//
// 两个信号任一落实即视为到账：reward 回查拿到发放记录，或额度总额高于
// 领取前的基线。两者都没落实时按「未确认」处理——宁可少报一次成功，
// 也不要让用户以为已经领到而实际没到账。
func confirmClaim(ctx context.Context, region Region, bearer string, identity MachineIdentity, target *campaignInfo, claim claimResponse, quotaBefore float64, hadQuotaBefore bool, result *CheckinResult) *CheckinResult {
	amount := 0
	if claim.Benefit != nil {
		amount = claim.Benefit.Amount
	}
	granted, rewardAmount := readCampaignReward(ctx, region, bearer, identity, target.CampaignID)
	if rewardAmount > 0 {
		amount = rewardAmount
	}
	quotaGrew := false
	if quotaAfter, errAfter := readAddonQuotaTotal(ctx, region, bearer); errAfter == nil && hadQuotaBefore {
		quotaGrew = quotaAfter > quotaBefore
	}

	switch {
	case granted || quotaGrew:
		result.Status = CheckinClaimed
		result.Amount = amount
		if amount == 0 {
			result.Message = fmt.Sprintf("领取成功（%s，上游未给出积分数）", target.CampaignKey)
		} else {
			result.Message = fmt.Sprintf("领取成功 +%d (%s)", amount, target.CampaignKey)
		}
		return result
	default:
		// 上游没有给出发放记录：可能是额度已发放但 reward 端点滞后，
		// 也可能是真的没发。两种情况都不该报成功。
		result.Status = CheckinNotConfirmed
		result.NotRecorded = true
		result.Message = fmt.Sprintf("上游未确认发放（%s），请稍后核对额度", target.CampaignKey)
		return result
	}
}

// readCampaignReward 回查活动的发放记录。
//
// 返回 (是否已发放, 发放积分数)。reward 端点对未领取的活动返回 404，
// 对已领取的返回 CLAIMED + grantedAt。
func readCampaignReward(ctx context.Context, region Region, bearer string, identity MachineIdentity, campaignID string) (bool, int) {
	path := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/reward", campaignID)
	status, body, _ := doCheckinRequest(ctx, region, http.MethodGet, path, bearer, identity, nil)
	if status != http.StatusOK {
		return false, 0
	}
	var reward rewardResponse
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &reward) != nil {
		return false, 0
	}
	if !strings.EqualFold(reward.Status, "CLAIMED") && reward.GrantedAt == "" {
		return false, 0
	}
	amount := 0
	if reward.Benefit != nil {
		amount = reward.Benefit.Amount
	}
	return true, amount
}

// readAddonQuotaTotal 读取拓展包额度总额（领取前的基线）。
func readAddonQuotaTotal(ctx context.Context, region Region, bearer string) (float64, error) {
	endpoints := GetEndpoints(region)
	raw, errQuota := httpGetBearerJSON(ctx, endpoints.QuotaEndpoint, bearer)
	if errQuota != nil {
		return 0, errQuota
	}
	bucket, okBucket := raw["addOnQuota"].(map[string]any)
	if !okBucket {
		return 0, nil
	}
	return toFloat(bucket, "total"), nil
}

// claimFailureSuffix 拼出上游拒绝原因后缀。
func claimFailureSuffix(claim claimResponse) string {
	switch {
	case claim.FailureCode != "":
		return "：" + claim.FailureCode
	case claim.ErrorCode != "":
		return "：" + claim.ErrorCode
	case claim.ErrorMessage != "":
		return "：" + claim.ErrorMessage
	default:
		return ""
	}
}

// campaignWindowCovers 判断活动窗口是否覆盖给定时刻。
//
// 上游每轮的窗口是 [当天 10:00, 次日 10:00)，因此 9 点这一轮覆盖的正是
// 「昨天 10:00 开启、今天 10:00 关闭」的那一轮——窗口仍然有效，但它是
// 上一轮。缺 startAt/endAt 时按「覆盖当下」处理：上游偶尔只给
// claimStatus，此时不能因为时间字段缺失就把一条 CLAIMABLE 活动判成不可领。
func campaignWindowCovers(campaign *campaignInfo, now time.Time) bool {
	if campaign.StartAt <= 0 {
		return true
	}
	start := time.Unix(campaign.StartAt, 0).In(campaignZone)
	if now.Before(start) {
		return false
	}
	if campaign.EndAt > 0 {
		end := time.Unix(campaign.EndAt, 0).In(campaignZone)
		if !now.Before(end) {
			return false
		}
	}
	return true
}

// isTodayRoundWindow 报告活动是否属于「今天这一轮」。
//
// 上游每轮的窗口是 [当天 10:00, 次日 10:00)，因此今天这一轮的开启时刻
// 就是**今天 10:00（UTC+8）**。判据因此是「活动的开启时刻不早于今天 10:00」：
//   - 9:00 时，上游下发的是昨天 10:00 开启的那一轮 → 早于今天 10:00 → 不是
//     今天这一轮（它虽仍覆盖当下，但属于上一轮）；
//   - 10:00 之后，上游下发的活动开启于今天 10:00 → 是今天这一轮。
//
// 这条判据把「窗口是否还开着」与「领的是哪一轮」彻底分开：前者决定能不能
// 领，后者决定领到了能不能算「今日已签到」。
func isTodayRoundWindow(campaign *campaignInfo, now time.Time) bool {
	if campaign.StartAt <= 0 {
		return true
	}
	local := now.In(campaignZone)
	todayOpen := time.Date(local.Year(), local.Month(), local.Day(), dailyRoundHour, 0, 0, 0, campaignZone)
	start := time.Unix(campaign.StartAt, 0).In(campaignZone)
	return !start.Before(todayOpen)
}

// campaignWindowDate 返回活动窗口的日期（UTC+8）。
//
// 用活动 startAt 而不是本地自然日：它标识的是「哪一轮」，正是幂等需要的键。
func campaignWindowDate(campaign *campaignInfo) string {
	if campaign.StartAt <= 0 {
		return ""
	}
	return time.Unix(campaign.StartAt, 0).In(campaignZone).Format("2006-01-02")
}

// achievementMessage 拼出「需先完成成就」的说明。
func achievementMessage(campaign *campaignInfo) string {
	if campaign.RequiredAchievementKey != "" {
		return fmt.Sprintf("需先完成成就 %s 才能领取", campaign.RequiredAchievementKey)
	}
	return "需先完成指定成就才能领取"
}

// unavailableSuffix 拼出上游给出的不可领取原因后缀。
func unavailableSuffix(campaign *campaignInfo) string {
	if campaign.UnavailableReason != "" {
		return "：" + campaign.UnavailableReason
	}
	return ""
}

// readDailyCheckinStats 只读 legacy 统计（绝不通过它领取）。
//
// 该接口对未领取日也返回 409，因此它的 status 不能用来判断「是否已领取」；
// 但它的连续天数与累计数据是准的，用来补充展示。
//
// 上游把该批次标为 DISABLED 时（实测国内版当前就是），返回的计数恒为 0。
// 那不是「连续 0 天」，而是这条统计线已经停用——此时不展示，避免用户
// 以为自己从没领过。
func readDailyCheckinStats(ctx context.Context, region Region, bearer string, identity MachineIdentity, result *CheckinResult) {
	status, body, _ := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/daily-check-in/status", bearer, identity, nil)
	if status != http.StatusOK {
		return
	}
	var parsed dailyCheckinStatus
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &parsed) != nil || parsed.Status == "" {
		return
	}
	if strings.EqualFold(parsed.Status, "DISABLED") {
		return
	}
	if parsed.CurrentStreakDays > result.StreakDays {
		result.StreakDays = parsed.CurrentStreakDays
	}
	if parsed.TotalClaimDays > result.TotalClaimDays {
		result.TotalClaimDays = parsed.TotalClaimDays
	}
	if parsed.TotalRewardCredits > result.TotalRewardCredits {
		result.TotalRewardCredits = parsed.TotalRewardCredits
	}
}

// parseCampaigns 把活动列表条目解成结构体。
func parseCampaigns(raw any) []campaignInfo {
	items, okItems := raw.([]any)
	if !okItems {
		return nil
	}
	out := make([]campaignInfo, 0, len(items))
	for _, item := range items {
		encoded, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			continue
		}
		var parsed campaignInfo
		if json.Unmarshal(encoded, &parsed) == nil {
			out = append(out, parsed)
		}
	}
	return out
}

// checkinHeaders 是桌面端签到所需的请求头（抓包确认）。
//
// cosy-clienttype: 10 是桌面端标识；上游按它放行签到接口，
// 不需要 cosy 签名（这是签到与对话接口最大的差异）。
//
// 机器头**只在用户提供了官方组件签发的真身份时才发**（见 identity.go）：
// 上游对 machinetoken/machinetype 做精确校验，任何派生、随机或拼接的值都会
// 让可领取活动被整条过滤——国内版本来看得见，发假头反而看不见。没有真身份
// 时一个都不发，这是唯一不会自我破坏的选择。
func checkinHeaders(bearer string, identity MachineIdentity) map[string]string {
	headers := map[string]string{
		"authorization":   "Bearer " + bearer,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
		"cosy-version":    identity.Version,
	}
	if !identity.Native() {
		return headers
	}
	headers["cosy-machinetoken"] = identity.Token
	headers["cosy-machinetype"] = identity.Type
	if identity.Code != "" {
		headers["cosy-machinecode"] = identity.Code
	}
	return headers
}

// checkinBase 返回该区域签到接口的域名。
//
// 必须按账号区域选：国际版 device token 打到国内域名会得到 401 TOKEN_EXPIRE
// （上游 qoder2api 写死了国内域名，因此国际版账号签到必然失败）。
func checkinBase(region Region) string {
	return GetEndpoints(region).SashBase
}

// doCheckinRequest 发送签到相关请求，返回 (状态码, 解析后的 JSON, 原始正文)。
func doCheckinRequest(ctx context.Context, region Region, method, path, bearer string, identity MachineIdentity, reqBody any) (int, any, string) {
	base := checkinBase(region)
	url := base + path
	var body io.Reader
	if reqBody != nil {
		encoded, errMarshal := json.Marshal(reqBody)
		if errMarshal != nil {
			return 0, nil, "encode request: " + errMarshal.Error()
		}
		body = bytes.NewReader(encoded)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, body)
	if errRequest != nil {
		return 0, nil, fmt.Sprintf("build request: %v", errRequest)
	}
	for key, value := range checkinHeaders(bearer, identity) {
		req.Header.Set(key, value)
	}
	if method == http.MethodPost {
		req.Header.Set("origin", base)
		if reqBody == nil {
			// 抓包确认 claim 请求无 body；显式置 0 避免上游把空 body 当异常。
			req.ContentLength = 0
		}
	}
	resp, errDo := hostbridge.Client(ctx, checkinHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Sprintf("do request: %v", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return resp.StatusCode, nil, "read response: " + errRead.Error()
	}
	var parsed any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	return resp.StatusCode, parsed, string(data)
}
