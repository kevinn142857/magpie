# Token 套餐验证记录 · 2026-10-30

环境：macOS / arm64，Go 1.26.8，Docker Desktop 的 Linux / arm64 容器。
没有新增生产依赖，也没有使用真实上游账号或业务密钥。

## 结果

| 检查 | 结果 |
| --- | --- |
| 修改文件的 gofmt、Git whitespace 检查 | 通过 |
| 独立面板三份 JavaScript 的 `node --check` | 通过 |
| 原生、Linux nogui、Windows 的 `go vet ./...` | 全部通过 |
| 原生、Linux nogui、Windows 的构建 | 全部通过 |
| Go 包测试 | 本次重新运行全部 81 个有测试的包，全部通过，见下方分次执行说明 |
| tokenpack、access、gateway 的专项普通与 race 回归 | 通过 |
| 简化面板 API、分享、Web guard/one-port 的专项 race 回归 | 通过 |
| 供应商 API、协议探测、定制 HTTP client 的普通及 race 回归 | 通过 |
| MCP 鉴权、协议错误、工具参数、供应商/套餐管理及核销 race 回归 | 通过 |
| 官方 MCP TypeScript SDK 1.32.2 实际 HTTP 连接及工具调用 | 通过，11 个工具及 1M 套餐创建 |
| Chromium / WebKit 独立控制台测试 | 30/30 通过，含五种语言和窄屏 |
| 既有 i18n 键及语言一致性检查 | 4/4 通过 |
| 本机实际 Docker 面板只读浏览器检查 | 通过，四个页面、供应商表单、四种协议，无 JS 错误 |
| Compose 变量、端口、数据卷配置检查 | 通过 |
| Docker 镜像构建与隔离端到端验证 | 通过 |

本次供应商面板与 MCP 增量完成后，重新运行了全部 Go 包：
除 gateway 外的 98 个包中，80 个有测试并通过，其余没有测试文件；
gateway 整包单独通过，测试耗时 **631.969 秒**。
使用项目 `.github/workflows/test.yml` 既有的 30 分钟包时限：

```sh
go test -count=1 -timeout=30m -p=2 -tags nogui ./internal/gateway
```

以上结果来自“除 gateway 外所有包 + 单独完整 gateway”，两次退出码均为 0。
最初版本曾在默认 10 分钟时限下中止 gateway，随后以 CI 时限完整通过；
本次直接使用 CI 时限，没有将超时当作通过。以后完整运行应使用：

```sh
go test -count=1 -timeout=30m -tags nogui ./...
```

`-count=1` 避免本机在大量临时目录读取记录上保存测试结果缓存。
Go 模块缓存和编译缓存仍被复用。所有 Go 测试均使用隔离 HOME，缓存路径固定，
没有读写用户实际 Agent 配置或正在运行的 Magpie 服务。

本次最终补跑的 GUI 专项 race 覆盖 `TestQuota*` 和 `TestTokenPack*`，
耗时 7.672 秒；供应商相关的 GUI/provider/catalog 专项 race 也全部通过。
三平台全量 vet、三平台构建及最终 GUI 的三平台 vet 均退出 0。

## 关键行为

- 一次性总额保存在独立 SQLite 账本，关闭数据库、清理展示日志和重启不会重置已用额度。
- 并发额度修改只更新提交字段，不会用旧启用状态覆盖停用操作。
- 每套餐同时只有一个请求；预留与结算幂等，执行中的请求不能被人工核销释放。
- 模型越权、停用、到期、耗尽、账本不可用均拒绝请求；拒绝记录不扣额。
- 缺少或不完整的 usage、未知缓存、流式中断、异常计数保留预留。
- 输入计数逐项校验，防止负数或整数溢出被误算成小额消费。
- 文本请求解压上限 1 MiB，并受网关共享内存准入约束；覆盖 gzip/zstd 和更小 Server 限制。
- 分享 token 与管理员凭证独立；分享页只展示该套餐，禁用或到期后不展示 Key。
- 管理 API 校验 JSON 字段、大小和来源；分享页有转义、nonce CSP、no-store 和 no-referrer。
- 不可信名称作为文本显示，日志不展示提示词、回复正文、上游错误正文或密钥。
- 网页直接新增、编辑、启停供应商；四种协议的模型获取和探测复用现有实现。
- 保存 Key 仅在协议和完整规范化 URL 都相同时复用；换域名、路径、端口、scheme 或协议需显式新 Key。
- 草稿模型获取和探测不落盘，拒绝重定向，15 秒超时，响应读取限制为 1 MiB。
- 供应商错误、模型返回和摘要不回显完整 Key；复杂供应商配置只读。
- MCP 不接受网页 Cookie、网页 Key 或套餐 Key；独立 Bearer 凭证、固定路径与参数白名单限制管理调用。
- MCP notification 不执行管理工具；非对象参数、数组 null、越界额度、路径注入均拒绝。
- MCP 通过相同账本执行额度修改和核销；不能减少已知消费或释放活动请求。

新增行为的 Red→Green 证据：供应商路由实现前
`TestQuotaProviderCreateListEditAndToggleWithoutSecrets` 返回 404；MCP 适配实现前
`TestQuotaMCPRequiresIndependentCredential` 的合法 MCP 凭证仍返回网页 guard 的 401。
三条 vendor 模型分支的 client 传播测试在修复前报告
`vendor list bypassed selected client`。随后普通测试和专项 race 均通过。

## Docker 端到端

测试镜像为 `magpie-packages:local`，镜像 ID：

```text
sha256:d3cf6e7e500efa2ecc37839867c81a16629849a58dfc8ad49cf6e4d1aececdf4
```

使用临时容器、临时命名卷、随机管理员凭证以及本机模拟上游，验证：

1. 未登录的管理员 API 和无 Key 的网关请求均返回 401。
2. 登录后只提供额度面板，原 Settings API 返回 404。
3. 创建套餐、独立分享、JSON 响应类型、CSRF 拒绝均正确。
4. 经真实容器网关调用模拟上游，扣除 **30 Token**，预留释放为 0，日志显示完成。
5. 其他模型返回 403；套餐停用后分享页返回 410 且不暴露 Key。
6. 重启后额度仍为已扣除状态，套餐停用状态和管理员登录保留。
7. 健康检查通过，运行 UID 为 65532，程序文件和根目录不可写。
8. 经网页 API 创建供应商、脱敏列表、获取模型和探测；供应商热修改、启停和重启持久化正确。
9. 上游地址或协议变化且缺少新 Key 时，保存、获取模型、探测均在发送前拒绝。
10. 官方 MCP SDK 的 `Client` + `StreamableHTTPClientTransport` 完成握手、发现 11 个工具、
    读取供应商/模型/套餐/日志、创建 1M 套餐，并验证分享页和余额持久化。

临时测试容器及卷已清理。单独启动的本机面板使用自己的持久化卷，
配置位于被 Git 忽略的 `.env`，没有留下演示套餐或模拟供应商。

## 验证边界

Windows 仅完成编译与 vet，没有实际 Windows 运行测试。Docker 实际验证平台是
Linux / arm64；没有在本机另外构建 amd64 镜像。macOS 构建有既有 SDK/链接器警告，
退出码为 0；Docker 构建不受这些原生 GUI 警告影响。

独立额度控制台在 Chromium 和 WebKit 中完成 30 项测试；旧桌面面板的完整
Playwright 套件未运行。既有 Go GUI 套件及 Web 鉴权相关回归已完成。

没有实际供应商密钥，因此真实模型调用未验收。首版的接口范围、串行请求、
保守输入估计和人工核销要求见 [部署与使用指南](token-packages.md)。
管理 MCP 支持 `2025-11-25`、`2025-06-18`、`2025-03-26`；不支持固定要求
`2026-07-28` 且禁用降级的客户端。官方 SDK 的实际联调协商到 `2025-11-25`。
