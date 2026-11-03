package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

type vpcLoginSessions struct {
	entries map[string]*core.LoginSession
}

func (s *vpcLoginSessions) Register(vendor, state string, _, _ time.Duration, data any) {
	if s.entries == nil {
		s.entries = map[string]*core.LoginSession{}
	}
	s.entries[state] = &core.LoginSession{VendorID: vendor, Data: data}
}

func (s *vpcLoginSessions) Poll(state string) (*core.LoginSession, core.PollState) {
	if entry := s.entries[state]; entry != nil {
		return entry, core.PollReady
	}
	return nil, core.PollMissing
}

func (s *vpcLoginSessions) Owns(state string) (string, bool) {
	entry := s.entries[state]
	if entry == nil {
		return "", false
	}
	return entry.VendorID, true
}

func (s *vpcLoginSessions) Delete(state string) { delete(s.entries, state) }

func TestVPCEndpointValidation(t *testing.T) {
	for _, value := range []string{"acme.vpc.qoder.com.cn", "https://acme.vpc.qoder.com.cn/", " HTTPS://ACME.vpc.qoder.com.cn "} {
		host, err := normalizeVPCEndpoint(value)
		if err != nil || host != "acme.vpc.qoder.com.cn" {
			t.Errorf("normalize %q = %q, %v", value, host, err)
		}
	}
	for _, value := range []string{
		"http://acme.vpc.qoder.com.cn", "https://user@acme.vpc.qoder.com.cn",
		"https://acme.vpc.qoder.com.cn:443", "https://acme.vpc.qoder.com.cn/account",
		"https://acme.vpc.qoder.com.cn?x=1", "https://acme.vpc.qoder.com.cn#login",
		"acme.vpc.qoder.com.cn.evil.test", "acme-openapi.vpc.qoder.com.cn",
		"acme-gateway.vpc.qoder.com.cn", "a.b.vpc.qoder.com.cn", "-acme.vpc.qoder.com.cn",
		"vpc.qoder.com.cn", "127.0.0.1", strings.Repeat("a", 56) + vpcDomainSuffix,
	} {
		if _, err := normalizeVPCEndpoint(value); err == nil {
			t.Errorf("accepted invalid endpoint %q", value)
		}
		if _, err := ParseCredential([]byte(`{"device_token":"dt-test","region":"cn","vpc_endpoint":`+strconv.Quote(value)+`}`), RegionCN); err == nil {
			t.Errorf("accepted credential with invalid endpoint %q", value)
		}
	}
	if _, err := resolveEndpoints(RegionGlobal, "acme.vpc.qoder.com.cn"); err == nil {
		t.Fatal("global account accepted a CN VPC endpoint")
	}
	if _, err := ParseCredential([]byte(`{"device_token":"dt-test","region":"cn","vpc_endpoint":123}`), RegionCN); err == nil {
		t.Fatal("accepted a non-string VPC endpoint")
	}
}

func TestVPCEndpointsAndPublicIsolation(t *testing.T) {
	for _, region := range []Region{RegionCN, RegionGlobal} {
		got, err := resolveEndpoints(region, "")
		if err != nil || got != GetEndpoints(region) {
			t.Fatalf("public endpoints changed for %s: %v", region, err)
		}
	}
	ep, err := resolveEndpoints(RegionCN, "acme.vpc.qoder.com.cn")
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{ep.PollEndpoint, ep.UserinfoBase, ep.PlanEndpoint, ep.QuotaEndpoint, ep.SashBase} {
		u, _ := url.Parse(endpoint)
		if u.Host != "acme-openapi.vpc.qoder.com.cn" {
			t.Errorf("wrong account host: %s", endpoint)
		}
	}
	for _, endpoint := range []string{ep.ChatStreamURL, ep.ModelListURL, ep.JobTokenURL} {
		u, _ := url.Parse(endpoint)
		if u.Host != "acme-gateway.vpc.qoder.com.cn" {
			t.Errorf("wrong gateway host: %s", endpoint)
		}
	}
	if ep.DeviceLoginBase != "https://acme.vpc.qoder.com.cn/device/selectAccounts" {
		t.Fatalf("wrong login URL: %s", ep.DeviceLoginBase)
	}
	a := &Credential{Token: "dt-test", Region: RegionCN}
	b := *a
	b.VPCEndpoint = "acme.vpc.qoder.com.cn"
	c := b
	c.VPCEndpoint = "other.vpc.qoder.com.cn"
	if CredentialFingerprint(a) == CredentialFingerprint(&b) || CredentialFingerprint(&b) == CredentialFingerprint(&c) {
		t.Fatal("public and enterprise credentials share a bridge cache key")
	}
}

func TestVPCLoginPersistsInstanceAndPKCE(t *testing.T) {
	ctx := context.Background()
	sessions := &vpcLoginSessions{}
	v := newInstance(core.Env{Logins: sessions}, RegionCN).(*adapter)
	// This test exercises login and storage without executing a desktop component.
	v.localIdentity.once.Do(func() {})
	start, err := v.LoginStart(ctx, map[string]any{"vpc_endpoint": "https://acme.vpc.qoder.com.cn/"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.URL)
	entry := sessions.entries[start.State].Data.(*pendingLogin)
	challenge := sha256.Sum256([]byte(entry.verifier))
	if u.Host != "acme.vpc.qoder.com.cn" || u.Query().Get("nonce") != entry.nonce ||
		u.Query().Get("challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) || u.Query().Get("client_id") != oauthClientID {
		t.Fatal("enterprise authorization lost the instance or PKCE binding")
	}
	ep, _ := resolveEndpoints(RegionCN, entry.vpcEndpoint)
	pollURL := ep.PollEndpoint + "?nonce=" + url.QueryEscape(entry.nonce) + "&verifier=" + url.QueryEscape(entry.verifier) + "&challenge_method=S256"
	upstream := &stubUpstream{responses: map[string]testhostResponse{
		"GET " + pollURL:         {http.StatusNotFound, `{}`},
		"GET " + ep.UserinfoBase: {http.StatusOK, `{"userId":"u1","email":"member@example.test","name":"Member","userType":"enterprise"}`},
		"GET " + ep.PlanEndpoint: {http.StatusOK, `{"plan_tier_name":"Enterprise"}`},
	}}
	installStubHost(t, upstream)
	waiting, err := v.LoginPoll(ctx, start.State)
	if err != nil || waiting.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("404 must keep waiting for authorization: %v", err)
	}
	upstream.responses["GET "+pollURL] = testhostResponse{http.StatusOK, `{"token":"dt-test","refresh_token":"drt-test"}`}
	result, err := v.LoginPoll(ctx, start.State)
	if err != nil || result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("login failed: %v", err)
	}
	cred, err := ParseCredential(result.Auth.StorageJSON, RegionCN)
	if err != nil || cred.VPCEndpoint != "acme.vpc.qoder.com.cn" || cred.RefreshToken != "drt-test" {
		t.Fatalf("instance not persisted: %+v, %v", cred, err)
	}
	if result.Auth.FileName != "qodercn-member@example.test-acme.json" || !strings.Contains(result.Auth.Label, "acme.vpc.qoder.com.cn") {
		t.Fatalf("enterprise account collides with public identity: %s / %s", result.Auth.FileName, result.Auth.Label)
	}
	for _, req := range upstream.requests {
		if !strings.Contains(req.url, "acme-openapi.vpc.qoder.com.cn/") {
			t.Errorf("enterprise login reached another host: %s", req.url)
		}
	}
	if _, ok := sessions.Owns(start.State); ok {
		t.Fatal("successful login session was not consumed")
	}
}

func TestVPCCredentialRefreshAndQuotaStayOnInstance(t *testing.T) {
	ctx := context.Background()
	cred := &Credential{Region: RegionCN, VPCEndpoint: "acme.vpc.qoder.com.cn", Token: "dt-old", RefreshToken: "drt-old", MachineToken: "machine"}
	ep, _ := resolveEndpoints(cred.Region, cred.VPCEndpoint)
	upstream := &stubUpstream{responses: map[string]testhostResponse{
		"POST https://acme-openapi.vpc.qoder.com.cn/api/v1/deviceToken/refresh": {http.StatusOK, `{"device_token":"dt-new","refresh_token":"drt-new"}`},
		"GET " + ep.UserinfoBase:  {http.StatusOK, `{"userId":"u1","name":"Member"}`},
		"GET " + ep.QuotaEndpoint: {http.StatusOK, `{"orgResourcePackage":{"cap":2000,"remaining":1500,"used":500,"available":true}}`},
		"GET " + ep.PlanEndpoint:  {http.StatusOK, `{"plan_tier_name":"Enterprise"}`},
		"GET " + ep.ModelListURL:  {http.StatusOK, `{"assistant":[{"key":"vpc-model","display_name":"VPC Model","enable":true}]}`},
	}}
	installStubHost(t, upstream)
	updated, changed, err := RefreshCredential(ctx, cred)
	if err != nil || !changed || updated.Token != "dt-new" || updated.RefreshToken != "drt-new" || updated.VPCEndpoint != cred.VPCEndpoint || updated.MachineToken != "machine" {
		t.Fatalf("refresh lost enterprise state: %+v, %v", updated, err)
	}
	var refreshBody map[string]string
	if err := json.Unmarshal([]byte(upstream.requests[0].body), &refreshBody); err != nil || refreshBody["refresh_token"] != "drt-old" {
		t.Fatal("refresh did not send the original refresh token")
	}
	quota, err := FetchQuota(ctx, updated)
	if err != nil || quota.OrgPackage == nil || quota.OrgPackage.Remaining != 1500 {
		t.Fatalf("enterprise quota not read: %+v, %v", quota, err)
	}
	bridge, err := NewBridge(ctx, updated.Token, RegionCN, nil, nil, updated.VPCEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	models, err := bridge.ListAvailableModels(ctx)
	if err != nil || len(models) != 1 || models[0].Key != "vpc-model" {
		t.Fatalf("enterprise models not read: %+v, %v", models, err)
	}
	before := len(upstream.requests)
	if result, err := Checkin(ctx, updated, MachineIdentity{}); err != nil || !result.NotRecorded || len(upstream.requests) != before {
		t.Fatal("enterprise credential was sent to public check-in services")
	}
	for _, req := range upstream.requests {
		u, _ := url.Parse(req.url)
		if u.Host != "acme-openapi.vpc.qoder.com.cn" && u.Host != "acme-gateway.vpc.qoder.com.cn" {
			t.Errorf("enterprise request reached another host: %s", req.url)
		}
	}
	upstream.responses["POST https://acme-openapi.vpc.qoder.com.cn/api/v1/deviceToken/refresh"] = testhostResponse{http.StatusUnauthorized, `{}`}
	if _, _, err := RefreshCredential(ctx, updated); err == nil {
		t.Fatal("revoked enterprise refresh token was accepted")
	}
}

func TestVPCChatUsesInstanceGateway(t *testing.T) {
	ep, err := resolveEndpoints(RegionCN, "acme.vpc.qoder.com.cn")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(AuthIdentity{Uid: "u1", SecurityOauthToken: "dt-test"}, "machine", "token", "type")
	if err != nil {
		t.Fatal(err)
	}
	template, err := TemplateBase()
	if err != nil {
		t.Fatal(err)
	}
	var requestedURL string
	hostbridge.Configure(func(_ string, method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			var req struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			requestedURL = req.URL
			return json.RawMessage(`{"status_code":200,"stream_id":"s1"}`), nil
		case pluginabi.MethodHostHTTPStreamRead:
			return json.RawMessage(`{"done":true}`), nil
		case pluginabi.MethodHostHTTPStreamClose:
			return json.RawMessage(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected RPC: %s", method)
		}
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	bridge := &Bridge{sess: sess, client: NewBearerClient(sess), region: RegionCN, endpoints: &ep, templateBase: template}
	err = bridge.CallQoder(context.Background(), "codex", nil, "qmodel", nil, func(Delta) {})
	if !errors.Is(err, ErrEmptyStream) || requestedURL != ep.ChatStreamURL {
		t.Fatalf("enterprise stream used %q, want %q (err=%v)", requestedURL, ep.ChatStreamURL, err)
	}
}
