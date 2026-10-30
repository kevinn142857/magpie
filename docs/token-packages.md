# Docker 部署与 Token 套餐

此 fork 在 Magpie 原有网关上提供简化面板：**额度概览、额度管理、日志**。
创建套餐时填写名称、允许模型和 Token 额度，生成独立 API Key 与分享链接。
接收者打开链接即可复制请求地址、Key 和模型；无需进入管理面板。

## 构建与启动

需要已运行的 Docker Engine 或 Docker Desktop，以及 Docker Compose v2。
在仓库根目录执行：

```sh
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

将生成的随机值填入 `.env` 的 `MAGPIE_WEB_KEY`。
设置 `MAGPIE_PUBLIC_URL` 为接收者实际访问的地址：本机试用可保留
`http://127.0.0.1:3430`；对外提供服务时填写自己的 HTTPS 域名，例如
`https://api.example.com`。这个地址用于生成分享链接和请求地址，
不包含管理员 Key，也不预先追加 `/v1`。

```sh
docker compose config --quiet
docker compose up -d --build
docker compose logs -f magpie
```

Compose 会检查这两个必要变量，未设置或为空时直接报错。
`MAGPIE_WEB_KEY` 至少 16 个字符，只接受字母、数字和 `- . _ ~`；
上面的命令生成 64 个十六进制字符，符合要求。

镜像从当前 fork 的源码构建，名称为 `magpie-packages:local`，
复用仓库的 `Dockerfile`，无需另外安装 Go 或前端构建工具。
`MAGPIE_QUOTA_PANEL=1` 默认启用简化面板，容器运行
`magpie web --addr 0.0.0.0:3430 --no-open`。

默认只把宿主机的 `127.0.0.1:3430` 发布给本机。
这个端口同时提供面板、套餐分享页和模型 API；内部网关的 3425 端口不对外发布。
要让局域网或服务器接口接收连接，在 `.env` 中明确设置
`MAGPIE_HOST=0.0.0.0`，或填写宿主机某个接口的 IP。
修改端口时同时修改 `MAGPIE_PORT` 和 `MAGPIE_PUBLIC_URL`。
HTTPS 反向代理可以转发到默认的 `127.0.0.1:3430`，并应允许流式响应。
代理必须保留浏览器请求的 `Host`，让它与 `Origin` 中的域名一致；
否则面板会以跨来源请求为由拒绝修改。Nginx 对应配置为
`proxy_set_header Host $http_host;`。仅设置 `X-Forwarded-Host` 不足以替代 `Host`。

## 管理员登录与配置上游

打开 `http://127.0.0.1:3430`，使用 `.env` 的 `MAGPIE_WEB_KEY` 登录。
服务器部署时打开自己的对外地址；远程服务器仍只绑定本机时，
可先使用 `ssh -L 3430:127.0.0.1:3430 user@server` 建立访问隧道。

此面板只管理套餐和用量。首次添加上游使用 Magpie 自带 CLI，
或使用已保存了供应商配置的 `/config` 数据卷。例如：

```sh
docker compose exec magpie magpie provider add "My Relay" \
  url=https://upstream.example.com/v1 \
  key=YOUR_UPSTREAM_API_KEY \
  models=YOUR_MODEL_ID

docker compose exec magpie magpie providers
```

上面所有域名、模型和 Key 都是占位值。替换为你拥有的真实上游配置。
`url` 是兼容 OpenAI Chat Completions 的上游 Base URL，通常以 `/v1` 结尾。
原生 Anthropic 接口可以改用 `anthropic=https://upstream.example.com`。
CLI 显示的供应商 ID 与模型 ID 构成面板中的模型名，例如 `my-relay/YOUR_MODEL_ID`。
修改供应商后若模型列表尚未刷新，刷新面板；必要时执行
`docker compose restart magpie`。上游 Key 留在数据卷中，套餐使用另行生成的网关 Key。

## 创建和分享套餐

1. 在“额度管理”创建套餐，填写名称、允许模型、Token 额度和可选有效期。
2. `1M` 表示 `1,000,000` 个 Token。套餐总额度用完不会按天或按月自动补满。
3. 复制生成的分享链接，发给接收者。持有链接的人可读取该套餐的 API Key。
   链接格式为 `https://api.example.com/share/<随机字符串>`，与管理员登录链接无关。
4. 接收者打开页面，复制协议对应的请求地址、Key 和模型到客户端。
   OpenAI 接口的 Base URL 为对外地址加 `/v1`；Anthropic 接口使用对外根地址。
5. 在“额度概览”查看用量，在“日志”查看请求记录；需要停止新请求时停用套餐。

管理员登录凭证 `MAGPIE_WEB_KEY` 与套餐的 API Key、分享链接是不同的凭证。
只向接收者发送套餐分享链接，不要发送管理面板的 `?k=...` 登录链接。
模型白名单在网关校验；分享页面不会暴露上游 Key 或其他套餐的配置。

### 首版支持的接口

套餐 Key 支持模型列表和四种文本生成协议：OpenAI Chat Completions、
OpenAI Responses、Anthropic Messages、Gemini GenerateContent（含流式响应）。
流式调用使用 HTTP/SSE；套餐 Key 暂不开放 WebSocket。
Codex 连接此网关时，应在模型供应商配置中设置 `supports_websockets = false`。
`count_tokens`（包括 Gemini 的 `countTokens`）、MCP、图片、音频、嵌入及其他工具接口
暂不向套餐 Key 开放；这些请求会被拒绝，不扣除套餐额度。
依赖 `count_tokens` 预估用量的客户端可能需要关闭预估，或改用兼容的客户端；
这不会关闭实际请求的 Token 记账。
首版套餐请求解压后的 JSON 请求体上限为 1 MiB（1,048,576 字节），
同时受网关的内存准入限制；大上下文请求可能在到达上游前被拒绝。

## Token 口径与异常核对

套餐使用独立的持久化账本，不使用 Magpie 原有按日、按周或按月重置的 Key 限额。
每次消耗为归一化后的输入、输出、缓存读取和缓存写入 Token 之和；这四项互斥，
上游报告的推理 Token 包含在输出中，不再重复累加。

为防止并发请求重复花费同一余额，**每个套餐同时只处理一个请求**。
请求开始前持久化预留该套餐全部剩余额度，并根据保守的输入估计限制输出长度。
请求完整结束且获得 usage 后，按实际 Token 结算并释放差额。
不同套餐可以同时使用。
客户端未明确指定输出上限时，默认最多生成 4096 个输出 Token。
可以在客户端申请更大的输出上限，但仍受可用余额约束。

上游缺少 usage、流式响应缺少完整终止标记、连接提前断开或进程崩溃时，账本保留预留，
将请求留在待核对状态，不自动按零消耗退款。管理员应确认请求已经结束，
根据上游记录核对实际消耗后处理这笔记录，再继续使用余额。
核销时填写该请求的实际总 Token 数（非负整数），
不要在用量未知时填写 `0` 来释放余额。容器重启不会清除未结算请求或恢复被预留的额度。
输入合计（含缓存）或输出计数缺失、为零，或缓存用量无法可靠归一化时，也按用量不完整
处理；即使上游明确返回某一项为 `0`，首版仍保守地留待核对。

这个机制不能保证任意上游、任意模型的供应商账单精确硬封顶：输入估计不等于
供应商完整计数，服务端也无法还原未返回的 usage。余额以本服务收到并核对的
Token 记录为依据。停用、到期和额度耗尽会阻止后续请求；已经执行的请求仍可能
完成并产生用量。

## 持久化、更新与停止

`magpie-config` 命名卷挂载到 `/config`，保存供应商、套餐 Key、分享配置和用量日志。
容器以 UID 65532 运行，命名卷由镜像提供初始目录权限。
重建容器不会清空这个数据卷。
升级时继续构建此 fork，不要替换成官方 Magpie 镜像或官方二进制：
官方版本不识别本 fork 的套餐账本，无法提供这里的总额度约束。
默认镜像以非 root 用户运行，程序所在目录不可写，因此不会自行替换为官方版本。

```sh
# 拉取自己的源码更新后重新构建。
docker compose up -d --build

# 停止并移除容器，保留配置与用量。
docker compose down
```

不要在需要保留数据时运行 `docker compose down -v`，它会删除命名卷。
`.env` 已被 Git 和 Docker 构建上下文排除，应另行妥善保留。
