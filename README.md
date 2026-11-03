# FreeTier 2API Plugin

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-Plugin-2496ED?style=flat)](https://github.com/router-for-me/CLIProxyAPI)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

将多个免费额度 AI 供应商接入 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的原生动态库插件，直接复用宿主的 OpenAI / Anthropic / Responses 兼容路由、鉴权、代理与负载均衡。

中文 | [English](README_EN.md)

> **企业 VPC 适配 Fork**：本仓库基于 [cxy2730 的维护版本](https://github.com/cxy2730/CLIProxyAPI-freetier2api-plugin)，新增 Qoder CN 企业专属实例登录。安装包见本仓库的 [Actions](https://github.com/starrylistener/CLIProxyAPI-freetier2api-plugin/actions)；构建成功后下载 `freetier-vpc-packages`，包含 Linux amd64 / arm64、Windows amd64 的 zip 和 `checksums.txt`。包内动态库仍使用插件 ID `freetier`，可覆盖现有安装。

如果这个项目帮到了你，欢迎点个 Star 支持一下。

> **本仓库说明**：上游 `jiandanc/CLIProxyAPI-freetier2api-plugin` 已随作者账号一并从 GitHub 消失（账号与仓库均为 404）。本仓库自 **v0.1.29** 起由 `cxy2730` 接手维护，插件 ID 仍为 `freetier`，已有安装可直接覆盖升级。仓库已从 fork 网络脱离，是独立仓库；完整提交历史保留，`v0.1.44` 及更早的 tag 只存在于归档仓库 `cxy2730/CLIProxyAPI-freetier2api-plugin-legacy`，本仓库从 `v0.1.45` 起继续发版。

## 功能特性

- 复用宿主 Anthropic `/v1/messages`、OpenAI `/v1/chat/completions` 与 OpenAI `/v1/responses` 接口，无需额外部署独立网关
- 支持 8 大体系共 15 个国内外免费额度供应商实例（AutoClaw 国内/国际与小浣熊已接入），凭证严格按供应商隔离，账号数量不设上限
- 模型清单动态探测、自定义模型别名与跨供应商同名模型自动负载均衡/故障转移
- 内嵌单页 Web 控制台：OAuth 设备授权 / 本地回调粘贴 / API Key 添加账号、账号别名、批量启停删除、额度可视化、签到、模型启停与设置（任务中心与运行日志页已移除，日志统一走宿主官方页）
- 请求体原样透传：不注入、不替换、不追加任何提示词，也不改写 `system` / `tools` / 采样参数
- 支持每日自动签到、每日自动任务与 Token 自动续期保活（签到记录只保留当天，跨天自动清空）
- 不在宿主面板注册 OAuth 登录项：插件不出现在 CLIProxyAPI「OAuth 登录」列表里，加账号与登录一律走插件控制台
- 纯 Go 标准库实现，零第三方运行时依赖

## 支持的供应商

| 供应商 ID | 展示名称 | 区域 | 认证方式 | 凭证文件名前缀 |
| --- | --- | --- | --- | --- |
| `workbuddycn` | WorkBuddy 国内版 | `cn` | OAuth 设备授权 | `workbuddycn-*.json` |
| `workbuddyglobal` | WorkBuddy 国际版 | `global` | OAuth 设备授权 | `workbuddyglobal-*.json` |
| `autoclawcn` | AutoClaw 国内版 | `cn` | API Key | `autoclawcn-*.json` |
| `autoclawglobal` | AutoClaw 国际版 | `global` | API Key | `autoclawglobal-*.json` |
| `qodercn` | Qoder 国内版 | `cn` | OAuth 设备授权 | `qodercn-*.json` |
| `qoderglobal` | Qoder 国际版 | `global` | OAuth 设备授权 | `qoderglobal-*.json` |
| `raccoon` | 小浣熊 | — | API Key | `raccoon-*.json` |
| `opencodezen` | OpenCode ZEN | `global` | API Key | `opencodezen-*.json` |
| `cline` | Cline | `global` | WorkOS 设备授权 | `cline-*.json` |
| `zcodecn` | ZCode 国内版 | `cn` | OAuth CLI 授权（智谱开放平台 bigmodel） | `zcodecn-*.json` |
| `zcodeglobal` | ZCode 国际版 | `global` | OAuth CLI 授权（z.ai） | `zcodeglobal-*.json` |
| `traecn` | Trae 国内版 | `cn` | OAuth 授权 | `traecn-*.json` |
| `traeglobal` | Trae 国际版 | `global` | OAuth 授权 | `traeglobal-*.json` |
| `codearts` | CodeArts | `cn` | OAuth 授权 | `codearts-*.json` |
| `minimaxcodecn` | MiniMax Code 国内版 | `cn` | OAuth 设备授权 | `minimaxcodecn-*.json` |
| `minimaxcodeglobal` | MiniMax Code 国际版 | `global` | OAuth 设备授权 | `minimaxcodeglobal-*.json` |

## 快速开始

### 方式一：通过 CPA 插件商店安装

本插件曾收录于 CLIProxyAPI 官方插件源，但插件源里的条目目前仍指向已失效的原仓库地址，一键安装会失败，待插件源更新后恢复：

1. 打开 CLIProxyAPI 管理面板 → **插件商店**。
2. 搜索 **FreeTier 2API** 并点击一键安装（条目恢复前请用方式二）。

### 方式二：手动安装 Release 动态库

从 [Releases](https://github.com/cxy2730/CLIProxyAPI-freetier2api-plugin/releases) 下载对应平台的归档包，解压后将动态库（`freetier.so` / `freetier.dll`）放入 CPA 插件目录，并在 `config.yaml` 中启用：

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    freetier:
      enabled: true
      enabled_realms: "cn,global"
```

### 方式三：源码编译

需 Go 1.22+ 与 C 编译器（启用 `CGO_ENABLED=1`），或直接使用仓库内置的 Docker 构建脚本（基于 `golang:1.24` Debian glibc 镜像）：

```bash
git clone https://github.com/cxy2730/CLIProxyAPI-freetier2api-plugin.git
cd CLIProxyAPI-freetier2api-plugin
./scripts/build.sh
```

编译产物输出至 `dist/freetier.<so|dylib|dll>`。

## 使用方法

### 1. 登录控制台并添加账号

启动 CLIProxyAPI 后，访问内嵌控制台页面：

```text
http://<CPA地址>:8317/v0/resource/plugins/freetier/console
```

- 点击 **「添加账号 ▾」** 选择对应供应商，按提示在浏览器完成 OAuth 授权或填入 API Key（账号数量不设上限）。
- 若在远程服务器 / Docker / WSL 部署，使用 Trae 或 CodeArts 登录时浏览器停留在 `127.0.0.1` 回调页，展开登录框下方的 **「回调没到插件？粘贴回调链接」**，将完整回调 URL 粘贴提交即可。

#### Qoder CN 企业专属实例（VPC）

使用 CPA **v8.0.49 或更新版本**。控制台选择 **Qoder 国内版**，在「企业专属域」填写管理员提供的登录域名，例如 `acme.vpc.qoder.com.cn`，点击「获取授权链接」，再打开链接完成企业认证。留空使用公共版。

专属域接受实例域名或 `https://实例名.vpc.qoder.com.cn/`，不能填写 `-openapi` / `-gateway` 服务域名。插件按官方 CN CLI 的规则派生账号与模型服务地址，并把 `vpc_endpoint` 保存到该账号的凭证 JSON。续期、额度、模型目录和对话均使用原实例，不受之后新增其他账号影响；同一人的公共账号和不同企业账号使用不同凭证文件名。企业账号暂不支持签到。

CPA 所在机器需能通过 HTTPS 访问实例的登录域名、`实例名-openapi.vpc.qoder.com.cn` 和 `实例名-gateway.vpc.qoder.com.cn`。浏览器可以打开登录页，只能证明浏览器的网络可达，不能替代 CPA 宿主的连通性检查。

### 2. 调用 API

账号添加后即可通过 CLIProxyAPI 标准接口调用。模型对外 ID 固定为 `<供应商前缀>-<上游模型名>`，前缀写死不可配置，例如 `qodercn-GLM-5.3`、`workbuddyglobal-deepseek-v4.1-flash`，完整清单用 `GET /v1/models` 取：

```bash
# OpenAI Chat Completions
curl http://localhost:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <CPA_API_KEY>" \
  -d '{"model":"qodercn-GLM-5.3","messages":[{"role":"user","content":"你好！"}]}'

# Anthropic Messages
curl http://localhost:8317/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -H "x-api-key: <CPA_API_KEY>" \
  -d '{"model":"qodercn-GLM-5.3-Flash","max_tokens":1024,"messages":[{"role":"user","content":"你好！"}]}'
```

### 3. 模型别名与跨供应商路由

在控制台「模型」表格中点击 **「模型 ID」** 列的名称或 `✎` 图标即可设置对外模型别名。将不同供应商的模型设为同一别名（例如把多家供应商的旗舰模型统一设为 `coding-max`），宿主会自动合并并在多个供应商间负载均衡与故障转移。

### 4. 账号别名

账号概览的「账号」列默认显示上游回传的昵称/邮箱，部分平台只给不可修改的默认昵称，多账号时很难分辨。点击该列的名称或 `✎` 图标即可设置自定义别名，编辑框会预填当前名称；展示顺序变为 **别名 → 上游昵称/邮箱 → 文件名 → 账号 ID**。留空保存、或填回原名称，都视为不设别名（直接清除记录、沿用上游名称），未设置别名时行为不变。

别名按**账号身份**（凭据文件名，即 `state.json` 里的 `account_aliases` 键）记录，与上游昵称、token 无关，因此 token 刷新、重登写回同一凭证文件后别名都不会丢。

## 配置说明

在宿主 `config.yaml` 的 `plugins.configs.freetier` 下可配置以下常用项（大多数运行期设置可直接在控制台页面调整并即时生效）：

| 配置键 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `enabled_realms` | string | `cn,global` | 启用的区域（`cn` / `global`） |
| `auto_checkin` | bool | `true` | 每日自动签到开关 |
| `auto_checkin_at` | string | `10:00` | 每日自动签到时间（`HH:MM`，本地时区）。排程粒度为整点，分钟不为 0 时顺延到下一整点，保证不会早于设定时刻执行 |
| `auto_tasks` | bool | `true` | 每日自动任务开关 |
| `codearts_credit_enabled` | bool | `false` | 明确开启 CodeArts 积分推理（消耗账号积分）；默认福利通道，失败不自动切换。修改后刷新模型目录 |
| `disabled_models` | string / list | 空 | 禁用模型列表（支持 `vendor:model` 或裸名，与控制台勾选合并） |
| `extra_models` | string | 空 | 额外补充注册的模型名（逗号分隔） |
| `state_dir` | string | `~/.freetier-plugin` | 状态与缓存目录（若存在 `~/.cli-proxy-api` 则默认使用其下 `freetier` 子目录） |
| `log_level` | string | `info` | 日志级别：`debug` / `info` / `error` |
| `log_to_file` | bool | `false` | 额外把日志写入 `<state_dir>/logs/`，按天分文件 |
| `log_to_host` | bool | `true` | 把插件日志镜像进宿主 `main.log`，CLIProxyAPI 面板的日志页可直接查看 |
| `host_log_file` | string | 自动探测 | 宿主日志文件路径。留空时按 `state_dir` 反推 `<base>/auths/logs/main.log` 或 `<base>/logs/main.log`，都不存在则不启用 |
| `host_log_level` | string | `info` | 镜像到宿主日志的最低级别：`debug` / `info` / `error` |

## 项目架构

```text
package main / cgo        C ABI 入口（main.go / cgo.go）
  └─ internal/app         组合根与 ABI 适配（生命周期、RPC 分发、五类能力）
       ├─ internal/admin    管理接口：HTTP 路由/响应 + 全局配置 + 模型目录 + 控制台页
       ├─ internal/tasks    供应商无关的任务目录、排程、队列与执行引擎
       ├─ internal/auth     账号枚举/去重/批量执行/额度格式化 + 登录会话登记
       ├─ internal/vendors/<name>   供应商协议与适配层（单包，文件名封闭白名单）
       │    ├─ adapter.go           [必备] core.Vendor 实现 + Instances/Configure/Match + ConfigFields
       │    ├─ chat.go              [必备] 协议端点/签名/请求头/载荷加工/传输/Chat/ChatStream/SSE
       │    ├─ credential.go        [必备] 凭证解析/归属判定/存储写回/设备身份/凭证辅助
       │    ├─ models.go            [必备] 模型目录/探测 + bundledModels() 兜底
       │    ├─ errors.go            [必备] 上游错误分类与可重试判定
       │    ├─ login.go             [必备] AuthMode + 登录流程 + 续期（只能有一个）
       │    ├─ tasks.go             [可选] 额度/签到/套餐领取/任务/每日奖励/账号资料
       │    ├─ *_test.go            [按需] 与源码同目录；实况测试用 *_live_test.go
       │    └─ assets/              [按需] go:embed 的内嵌资源
       └─ internal/core            供应商无关契约（Vendor、能力接口、注册表、凭证、错误分类）
```

- 供应商包只允许依赖 `core`（契约）+ `hostbridge`（宿主通信）+ `logger`（日志），多一个都不行。
- `internal/core` 只依赖标准库；`internal/app` 是最上层，且只有 `internal/app/vendors.go` 可以 import 具体供应商。
- 新增内部包必须显式声明分层边界（`layerRules` / `internalPackages`）。
- 根包（C ABI）的单测与源码同目录（`cgo_test.go`；宿主内存 / 只读页用例在 POSIX 上的 `cgo_hostbuffer_test.go`），不放进 `tests/`。

## 致谢与第三方说明

本项目的宿主契约、部分供应商协议研究与模型元数据参考或衍生自以下开源项目，特此致谢：

- **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)**（MIT）：宿主插件 ABI/API 契约（`cpasdk/`）与 HTTP/SSE 桥接设计。
- **[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)** 与 **[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)**（MIT）：WorkBuddy 协议适配、提示词过滤与日常任务体系参考。
- **[aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API)**：AutoClaw 与小浣熊协议适配（头集合/签名、proxy 通道、授权登录、模型目录、积分与签到口径）参考。

## 参与贡献

欢迎友好交流。遇到问题时，建议先让 Claude Code、Codex 等工具帮忙排查一下，大部分问题都能自己解决。如果能直接提个 PR 就更好了。

## 许可证

[MIT](LICENSE)
