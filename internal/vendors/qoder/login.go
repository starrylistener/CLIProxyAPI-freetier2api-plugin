package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件实现 Qoder 的 OAuth 设备授权登录（PKCE）。
//
// 流程（与上游 Qoder 桌面端一致）：
//  1. 生成 PKCE verifier/challenge 与 nonce，拼出授权链接（DeviceLoginBase）；
//  2. 用户在浏览器里完成授权；
//  3. 轮询 poll 端点：404 = 还没授权，200 = 拿到 device_token；
//  4. 取账号信息与套餐名，组装凭证 JSON。
//
// 与 WorkBuddy 的差异：WorkBuddy 是自有 state/token 三接口，Qoder 是标准
// PKCE 设备码流。两者形状完全不同，因此登录协议属于供应商层。

const (
	// oauthClientID 是 Qoder 桌面端使用的公开 client id（与上游实现一致）。
	oauthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// loginFlowTTL 是单次登录会话的有效期（与上游 10 分钟一致）。
	loginFlowTTL = 10 * time.Minute
	// loginHTTPTimeout 是登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
	// loginPollInterval 是同一会话两次访问上游 poll 端点之间的最小间隔。
	//
	// 控制台页的轮询频率远高于上游建议（上游客户端是 1 秒一次），
	// 不节流会让面板一开就把上游打满。
	loginPollInterval = 1200 * time.Millisecond
)

// pendingLogin 是一次进行中的登录会话数据（TTL / 节流由 auth 会话表统一管理）。
type pendingLogin struct {
	nonce       string
	verifier    string
	region      Region
	vpcEndpoint string
}

// LoginProfile 是账号信息（取不到时保持零值）。
type LoginProfile struct {
	UserID string
	Email  string
	Name   string
	Type   string
}

// LoginStart 生成 PKCE 参数与登录 URL，并登记一个待轮询的登录会话。
func LoginStart(ctx context.Context, logins core.LoginSessions, region Region, vpcEndpoint ...string) (loginURL, state string, expiresAt time.Time, err error) {
	host, errEndpoint := normalizeVPCEndpoint(firstVPCEndpoint(vpcEndpoint))
	if errEndpoint != nil {
		return "", "", time.Time{}, errEndpoint
	}
	endpoints, errEndpoint := resolveEndpoints(region, host)
	if errEndpoint != nil {
		return "", "", time.Time{}, errEndpoint
	}
	verifier, challenge, errPKCE := core.NewPKCEPair()
	if errPKCE != nil {
		return "", "", time.Time{}, errPKCE
	}
	nonce := core.RandomHex(16)
	state = core.RandomHex(16)

	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", oauthClientID)
	loginURL = endpoints.DeviceLoginBase + "?" + params.Encode()

	deadline := time.Now().Add(loginFlowTTL)
	logins.Register(VendorIDFor(region), state, time.Until(deadline), loginPollInterval, &pendingLogin{
		nonce:       nonce,
		verifier:    verifier,
		region:      region,
		vpcEndpoint: host,
	})
	return loginURL, state, deadline, nil
}

// LoginPollResult 是一次轮询的结果。
type LoginPollResult struct {
	// Pending 为 true 表示还在等用户授权，调用方应继续轮询。
	Pending bool
	// Token / RefreshToken / Profile 在拿到凭证时有效。
	Token        string
	RefreshToken string
	Profile      LoginProfile
	// Region 是本次登录的区域（取凭证时要用它拼落盘内容）。
	Region      Region
	VPCEndpoint string
}

// LoginPoll 轮询一次登录会话。
//
// 返回错误表示会话已失效（不存在或超时），调用方应终止轮询；
// 返回 Pending=true 表示用户还没完成授权，继续轮询即可。
func LoginPoll(ctx context.Context, logins core.LoginSessions, state string) (LoginPollResult, error) {
	session, pollState := logins.Poll(state)
	if pollState == core.PollMissing {
		return LoginPollResult{}, fmt.Errorf("登录会话不存在或已过期，请重新发起登录")
	}
	// 节流：面板轮询频率高于上游建议（上游客户端是 1 秒一次）。
	if pollState == core.PollThrottled {
		return LoginPollResult{Pending: true}, nil
	}
	entry, okEntry := session.Data.(*pendingLogin)
	if !okEntry {
		return LoginPollResult{}, fmt.Errorf("登录会话数据已损坏，请重新发起登录")
	}

	token, refreshToken, waiting, errPoll := pollDeviceToken(ctx, entry)
	if errPoll != nil {
		// 网络抖动不该直接判死：保持会话，下次轮询再试。
		return LoginPollResult{Pending: true}, nil
	}
	if waiting {
		return LoginPollResult{Pending: true}, nil
	}

	profile := fetchLoginProfile(ctx, token, entry.region, entry.vpcEndpoint)
	logins.Delete(state)
	return LoginPollResult{
		Token:        token,
		RefreshToken: refreshToken,
		Profile:      profile,
		Region:       entry.region,
		VPCEndpoint:  entry.vpcEndpoint,
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商创建且仍有效。
func OwnsLoginSession(logins core.LoginSessions, sessionID string) bool {
	_, okEntry := logins.Owns(sessionID)
	return okEntry
}

// pollDeviceToken 访问上游 poll 端点。
//
// waiting=true 表示用户尚未授权（上游 404），调用方应继续等待。
func pollDeviceToken(ctx context.Context, entry *pendingLogin) (token, refreshToken string, waiting bool, err error) {
	endpoints, errEndpoints := resolveEndpoints(entry.region, entry.vpcEndpoint)
	if errEndpoints != nil {
		return "", "", false, errEndpoints
	}
	pollURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		endpoints.PollEndpoint, url.QueryEscape(entry.nonce), url.QueryEscape(entry.verifier))
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if errRequest != nil {
		return "", "", false, errRequest
	}
	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", false, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return "", "", false, fmt.Errorf("读取上游 poll 响应失败：%w", errRead)
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		// 上游语义：用户还没授权。
		return "", "", true, nil
	case http.StatusOK:
	default:
		return "", "", false, fmt.Errorf("上游 poll 返回 HTTP %d：%s", resp.StatusCode, summarizeBody(body))
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return "", "", false, fmt.Errorf("上游 poll 响应不是合法 JSON：%w", errUnmarshal)
	}
	deviceToken, _ := payload["token"].(string)
	deviceToken = strings.TrimSpace(deviceToken)
	if deviceToken == "" {
		return "", "", false, fmt.Errorf("上游 poll 响应里没有 token")
	}
	refreshToken, _ = payload["refresh_token"].(string)
	return deviceToken, strings.TrimSpace(refreshToken), false, nil
}

// fetchLoginProfile 取账号信息（失败只降级：没有 email/name 也能用）。
func fetchLoginProfile(ctx context.Context, token string, region Region, vpcEndpoint ...string) LoginProfile {
	endpoints, errEndpoints := resolveEndpoints(region, firstVPCEndpoint(vpcEndpoint))
	if errEndpoints != nil {
		return LoginProfile{}
	}
	result, errProfile := httpGetBearerJSON(ctx, endpoints.UserinfoBase, token)
	if errProfile != nil {
		return LoginProfile{}
	}
	return LoginProfile{
		UserID: core.StringField(result, "userId"),
		Email:  strings.ToLower(core.StringField(result, "email")),
		Name:   core.StringField(result, "name"),
		Type:   core.StringField(result, "userType"),
	}
}

// FetchPlanName 取套餐名（失败留空）。
func FetchPlanName(ctx context.Context, token string, region Region, vpcEndpoint ...string) string {
	endpoints, errEndpoints := resolveEndpoints(region, firstVPCEndpoint(vpcEndpoint))
	if errEndpoints != nil {
		return ""
	}
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	return core.StringField(result, "plan_tier_name")
}

// BuildLoginStorage 生成落盘用的凭证 JSON（扁平格式）。
//
// 与导入路径同构：刷新时代码（auth.refresh）能原样读回这些字段。
func BuildLoginStorage(region Region, token, refreshToken string, profile LoginProfile, plan string, identity MachineIdentity, vpcEndpoint ...string) ([]byte, *Credential) {
	payload := map[string]any{
		"device_token": token,
		"region":       string(region),
		"auth_mode":    "oauth",
	}
	if refreshToken != "" {
		payload["refresh_token"] = refreshToken
	}
	host := firstVPCEndpoint(vpcEndpoint)
	if host != "" {
		payload["vpc_endpoint"] = host
	}
	label := profile.Name
	if label == "" {
		label = profile.Email
	}
	if host != "" {
		label += " (" + host + ")"
	}
	if label != "" {
		payload["label"] = label
	}
	if profile.Email != "" {
		payload["email"] = profile.Email
	}
	if plan != "" {
		payload["plan"] = plan
	}
	// 设备身份随凭证一起落盘：国际版活动只下发给登录过官方客户端的设备，
	// 上游按 machinetoken 精确校验。登录时能拿到本机官方组件签发的身份，
	// 就一并写进凭证，用户不必再手工把 runtime-info 的输出抄进配置。
	if identity.Native() {
		payload["machine_token"] = identity.Token
		payload["machine_type"] = identity.Type
		if identity.Code != "" {
			payload["machine_code"] = identity.Code
		}
	}
	if profile.UserID != "" {
		payload["qoder_account_id"] = profile.UserID
	}
	if profile.Type != "" {
		payload["qoder_user_type"] = profile.Type
	}
	storage, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		// map 里只有字符串，理论上不会失败；退化成紧凑编码保证登录不中断。
		storage, _ = json.Marshal(payload)
	}
	return storage, &Credential{
		Token:        token,
		RefreshToken: refreshToken,
		Region:       region,
		VPCEndpoint:  host,
		Label:        label,
		Email:        profile.Email,
		MachineToken: identity.Token,
		MachineType:  identity.Type,
		MachineCode:  identity.Code,
	}
}

// LoginAuthID 生成宿主侧 auth 标识（会用于落盘文件名，必须是文件名安全字符）。
func LoginAuthID(profile LoginProfile, state string) string {
	base := profile.Email
	if base == "" {
		base = profile.UserID
	}
	if base == "" {
		base = "login-" + shortState(state)
	}
	return "qoder-" + sanitizeAuthID(base)
}

// shortState 截断 state 用于日志（完整 state 是凭证材料的一部分）。
func shortState(state string) string {
	if len(state) <= 8 {
		return state
	}
	return state[:8]
}

// sanitizeAuthID 只保留文件名安全字符。
func sanitizeAuthID(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	out := strings.Trim(builder.String(), "-.@")
	if out == "" {
		out = "account"
	}
	return out
}

// summarizeBody 截断响应正文用于错误信息。
func summarizeBody(body []byte) string {
	return core.Truncate(string(body), 200)
}

// 编译期确认 bridge 的凭证解析在本包可用（登录落盘要用它解回 token）。

// 本文件实现 Qoder 的凭证续期（校验 + 刷新）。
//
// 两条路径的语义差别：
//   - PAT（pt-…）走 jobToken 交换，**会拿到新 refreshToken**；
//   - 设备令牌（dt-…）用 userinfo 保活，内容不变时不写盘
//     （否则 mtime 会无意义地变动，用户无法判断是否真的续期过）。
//
// refreshed=false 表示仅校验通过、上游没下发新凭证。

// RefreshCredential 校验并刷新凭证。
func RefreshCredential(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, fmt.Errorf("credential is nil")
	}
	endpoints, errEndpoints := resolveEndpoints(cred.Region, cred.VPCEndpoint)
	if errEndpoints != nil {
		return nil, false, errEndpoints
	}
	deviceToken, refreshToken := ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		// 设备令牌：用 userinfo 保活，内容不变。
		if cred.VPCEndpoint != "" && cred.RefreshToken != "" {
			return refreshVPCDeviceToken(ctx, cred, endpoints)
		}
		if _, errInfo := FetchUserInfoWithToken(ctx, deviceToken, cred.Region, cred.VPCEndpoint); errInfo != nil {
			return nil, false, ClassifyCredentialError(errInfo)
		}
		return cred, false, nil
	}

	// PAT：换 jobToken 拿新的 securityOauthToken / refreshToken。
	seed := FingerprintSeed("", cred.Token)
	jobToken, errExchange := ExchangeJobToken(ctx, cred.Token,
		DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return nil, false, ClassifyCredentialError(errExchange)
	}
	oauthToken := StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return nil, false, NewError("qoder_credential_invalid",
			"jobToken 响应里没有 securityOauthToken", 401)
	}
	nextRefresh := StrVal(jobToken, "refreshToken")
	if nextRefresh == "" {
		nextRefresh = refreshToken
	}
	if oauthToken == cred.Token && nextRefresh == cred.RefreshToken {
		// 上游回了同样的值：算「有效」但不算「已刷新」，避免无意义写盘。
		return cred, false, nil
	}
	updated := &Credential{
		Token:        oauthToken,
		RefreshToken: nextRefresh,
		Region:       cred.Region,
		VPCEndpoint:  cred.VPCEndpoint,
		Label:        cred.Label,
		Email:        cred.Email,
		// 设备身份必须跟着续期走：它是账号能看见国际版活动的前提，
		// 续期时丢掉等于每刷新一次就把活动权限清零（旧实现的真实缺陷）。
		MachineToken: cred.MachineToken,
		MachineType:  cred.MachineType,
		MachineCode:  cred.MachineCode,
	}
	return updated, true, nil
}

// refreshVPCDeviceToken keeps rotating device credentials on the same instance.
func refreshVPCDeviceToken(ctx context.Context, cred *Credential, endpoints Endpoints) (*Credential, bool, error) {
	body, err := json.Marshal(map[string]string{"refresh_token": cred.RefreshToken})
	if err != nil {
		return nil, false, err
	}
	endpoint := strings.TrimSuffix(endpoints.PollEndpoint, "/poll") + "/refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, err := readQoderResponseBody(resp.Body)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, NewError("qoder_refresh_failed", fmt.Sprintf("企业 Token 续期失败（HTTP %d），请重试或重新登录", resp.StatusCode), resp.StatusCode)
	}
	var result struct {
		Token        string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, false, fmt.Errorf("企业 Token 续期响应无效")
	}
	if strings.TrimSpace(result.Token) == "" {
		return nil, false, fmt.Errorf("企业 Token 续期响应缺少 device_token")
	}
	updated := *cred
	updated.Token = result.Token
	if strings.TrimSpace(result.RefreshToken) != "" {
		updated.RefreshToken = result.RefreshToken
	}
	return &updated, updated.Token != cred.Token || updated.RefreshToken != cred.RefreshToken, nil
}
