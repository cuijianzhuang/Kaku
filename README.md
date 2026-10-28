<p align="center"><img src="docs/logo.svg" width="96" height="96" alt="Kaku"></p>

<h1 align="center">Kaku</h1>

自托管的通用发卡系统：卖卡密、账号、兑换码等虚拟商品，支付成功后自动发货。同时是 [Polaris](https://github.com/cuijianzhuang/Polaris) 专业版激活码的售卖渠道——付款后实时向 polaris-license 签发激活码，续费、退款吊销全自动。

- **Go 单二进制**，内置网页商店、管理后台和 Telegram 机器人，默认 SQLite，可切 PostgreSQL
- **免登录下单**：邮箱 + 查询密码，凭订单号或邮箱查单
- **支付**：易支付（支付宝 / 微信）、USDT-TRC20（唯一尾数金额自动对账）、Telegram Stars
- **发货方式插件化**：卡密库存、固定文本、人工、Webhook、Polaris 激活码；失败按 1m / 5m / 30m / 2h 自动重试
- **安全**：卡密 AES-256-GCM 加密存储、查询密码和后台密码 Argon2id、支付回调验签 + 金额比对 + 主动查单、后台 TOTP 两步验证与审计日志

## 快速开始

### 方式一：一键安装（Linux + systemd，推荐）

准备一台 amd64 / arm64 / armv7 的 Linux 服务器，把域名解析到它，以 root 执行：

```bash
bash <(curl -fsSL https://github.com/cuijianzhuang/Kaku/releases/latest/download/install.sh) install --domain shop.example.com
```

脚本会下载最新版本并校验 SHA-256，生成主密钥、管理员密码和随机后台路径，配置 systemd 服务，并安装 Caddy 自动申请 HTTPS 证书。完成后打印商店地址、后台地址和密码（密码只显示一次）。

| 项目 | 位置 |
| --- | --- |
| 配置（含主密钥） | `/etc/kaku/kaku.env`，请离线备份 |
| 数据 | `/var/lib/kaku` |
| 管理命令 | `kaku-ctl status \| log \| update \| port \| backup \| info \| reset-password \| uninstall` |

常用选项：`--port N`（默认 8090，被占用时自动顺延到下一个空闲端口；与 polaris-license 的 8080 错开，可装在同一台服务器）、`--version vX.Y.Z`、`--no-caddy`（自备反向代理）。装好后要换端口执行 `kaku-ctl port 新端口`，会同时更新 Caddy 和防火墙。不带 `--domain` 时以 HTTP 监听公网端口，仅适合测试。服务器访问 GitHub 慢时可设置下载代理前缀：`GH_PROXY=https://你的代理/ bash <(curl …) install …`。

`kaku-ctl update` 升级到最新版本，新版本启动失败会自动回滚；`kaku-ctl backup` 把数据库和配置打包到 `/root`。

#### 管理员、图片与分享

- **系统 → 管理员**：添加或删除管理员、重置其他管理员的密码（都需要输入你自己的密码），查看每个人的两步验证状态、绑定的第三方账号和最后登录时间。所有管理员权限相同。
- 商品封面和商店 Logo 可以直接上传（PNG / JPG / GIF / WebP，5 MB 以内），保存在数据目录的 `uploads/`；`kaku-ctl backup` 会一并打包。
- 商品页带分享卡片信息（标题、价格、简介、封面），发到微信、Telegram 时显示预览；另有 `robots.txt` 和 `sitemap.xml`（订单、查单等私密页面不收录）。

#### 主题与外观

**设置 → 外观** 有 6 套主题：简约、纸张（暖白纸色、衬线标题）、海洋、森林、樱花（大圆角）、午夜（始终深色）。每套都有浅色和深色两版，可选「跟随系统 / 浅色 / 深色」，强调色可单独覆盖。商店和后台同时生效。默认使用 Kaku 图标，也可以填写自己的 Logo 图片地址。

#### GitHub / Google 登录后台

**设置 → 登录方式** 填写 OAuth 应用的 Client ID 和 Secret（页面上有回调地址），登录页就会出现「使用 GitHub 登录」「使用 Google 登录」按钮。每个管理员先用密码登录，在 **安全** 页面绑定自己的账号，之后就能直接用该账号登录。只有已绑定的账号能登录，不会自动创建管理员；开启了两步验证的仍需输入验证码。商店顶部导航和页脚默认显示「后台」登录入口；想隐藏后台地址，可在同一页关闭，之后只能直接输入后台地址（`kaku-ctl info` 可查看）登录。

#### 在线升级与定时升级

后台 **系统 → 升级** 显示当前版本和最新版本，可一键升级；也可以开启「自动安装新版本」，每天在设定的整点检查，有新版本就自动升级，并在有新版本、升级完成或失败时通知 Telegram 管理员。

每次升级前会先备份数据库（SQLite），然后下载新版本、校验 SHA-256、重启服务；新版本无法启动时自动回滚到旧版本。某个版本升级失败后不会自动重试，可以在页面查看升级日志。

商店进程以普通用户运行，不能替换自己的程序：后台只写入一个升级请求（`/var/lib/kaku/update-request`，只含版本号），由 root 运行的 `kaku-update.path` 调用 `kaku-ctl apply-request` 完成升级，结果写到 `/var/lib/kaku-updater`。用旧版脚本安装的服务器执行一次 `kaku-ctl update` 即可启用。Docker 部署请用 `docker compose pull && docker compose up -d`（定时升级可用 Watchtower）。

### 方式二：Docker Compose

```bash
mkdir kaku && cd kaku
base=https://github.com/cuijianzhuang/Kaku/releases/latest/download
curl -fsSLO $base/compose.yaml && mkdir deploy && curl -fsSL -o deploy/Caddyfile $base/Caddyfile
curl -fsSL -o .env $base/kaku.env.example
docker run --rm ghcr.io/cuijianzhuang/kaku keygen   # 填到 .env 的 KAKU_MASTER_KEY，并另外备份
vi .env                  # 填 ADMIN_PASSWORD（≥12 位），建议改 ADMIN_PATH
vi deploy/Caddyfile      # 把 shop.example.com 换成你的域名
docker compose up -d
```

镜像 `ghcr.io/cuijianzhuang/kaku` 支持 amd64 / arm64，标签有 `latest`、`1.2.3`、`1.2`。

### 方式三：二进制

从 [Releases](https://github.com/cuijianzhuang/Kaku/releases) 下载对应系统的文件（Linux / macOS / Windows / FreeBSD），然后：

```bash
export KAKU_MASTER_KEY=$(./kaku keygen)   # 务必保存
export ADMIN_PASSWORD='至少12位的密码'
./kaku serve                               # 默认 :8080，数据在 ./data
```

### 上架第一个商品

1. **设置 → 站点**：填写站点地址（HTTPS），支付回调、邮件链接、续费链接都依赖它。
2. **设置 → 支付**：填写易支付网关 / 商户号 / 密钥，或 USDT 收款地址。
3. **设置 → 邮件**：填写 SMTP，买家会收到发货邮件。
4. **商品 → 管理分类**，新增分类。
5. **商品 → 从模板创建**：选一种常见商品，商品和规格（含发货方式）一键建好，暂不上架：
   | 模板 | 发货方式 | 创建后 |
   | --- | --- | --- |
   | 卡密 / 激活码、账号密码 | 卡密库存 | 跳到 **卡密** 页，粘贴或上传 TXT / CSV 导入（账号可一行「账号----密码」） |
   | 资源下载 / 教程 | 固定文本 | 填写下载地址；可写 `{order_no}` `{email}` `{qty}`，发货时替换 |
   | 人工代充、人工服务 | 人工发货 | 下单时买家填写「充值账号 / 联系方式」，付款后在订单页填写发货内容 |
   | 接口发货 | Webhook | 填写你的接口地址，签名密钥已生成 |

   也可以 **新增商品** 后手动 **新增规格**。每种发货方式都有自己的表单，不需要写 JSON；批发价按「满 N 件按原价 X%」填写。
6. 补全内容后，把商品状态改为上架。

## 环境变量

| 变量 | 说明 |
| --- | --- |
| `KAKU_MASTER_KEY` | **必填**。32 字节主密钥（`kaku keygen`）。加密卡密、发货内容、支付密钥；丢失后无法解密 |
| `ADMIN_USER` / `ADMIN_PASSWORD` | 首次启动创建的管理员；`kaku reset-admin` 也用它们重置密码并关闭两步验证 |
| `ADMIN_PATH` | 后台路径，默认 `/admin` |
| `LISTEN` | 监听地址，默认 `:8080` |
| `DATA_DIR` | 数据目录，默认 `data` |
| `DB_DRIVER` / `DB_DSN` | `sqlite`（默认，`$DATA_DIR/kaku.db`）或 `postgres` + 连接串 |
| `TRUST_PROXY=1` | 在 Caddy / Nginx 后面时，从 `X-Forwarded-For` 取客户端 IP |
| `SECURE_COOKIE=1` | Cookie 只走 HTTPS |

其余配置（支付、SMTP、Telegram、授权服务、Turnstile 人机验证）都在后台「设置」页，密钥类字段加密保存。

## 订单流程

```
pending ──支付回调验签+金额一致──▶ paid ──发货成功──▶ delivered
   │                                 │
   └─超时（默认 15 分钟）▶ expired     └─重试耗尽▶ failed（告警，后台可重新发货 / 手动发货 / 退款）
任意已付款状态 ──后台退款──▶ refunded（Polaris 授权自动吊销，Stars 原路退回）
```

- 只有「验签通过 + 金额一致 + 订单仍待支付」才会发货；`payments.trade_no` 唯一索引挡住重复回调，订单状态用条件更新防止重复发货。
- 易支付回调还会调用网关 `api.php?act=order` 二次确认。
- 库存型商品下单时预占卡密（PostgreSQL 用 `FOR UPDATE SKIP LOCKED`，SQLite 单写者串行），超时释放；超时后才付款的订单只要还有库存仍会发货。
- 同一 IP / 邮箱 / Telegram 账号的未付订单默认最多 3 单；查单同 IP 15 分钟内失败 10 次锁定。

## 对接 Polaris（polaris-license）

Kaku 不持有签名私钥，只用一个共享密钥调用 polaris-license 的管理接口：

| 接口 | 用途 |
| --- | --- |
| `GET /admin/v1/catalog` | 读取套餐、功能和许可证模板（「测试连接」和商品配置用） |
| `POST /admin/v1/keys` | 新购：签发激活码（order_no 做幂等键；离线 SKU 返回 .lic 文本） |
| `POST /admin/v1/licenses/{id}/extend` | 续费：延长同一许可证，面板下次校验即生效，无需重新激活 |
| `POST /admin/v1/licenses/{id}/revoke` | 退款：吊销，面板下次校验回落社区版 |
| `GET /admin/v1/licenses/{id}` | 后台同步状态 |
| 回调 `POST {kaku}/hooks/polaris` | 激活 / 停用事件（可选） |

请求签名：`X-Kaku-Timestamp`（Unix 秒）+ `X-Kaku-Signature = hex(HMAC-SHA256(secret, ts + "." + METHOD + " " + PATH + "." + body))`，时间窗 5 分钟。签名里包含方法和路径，防止把针对 A 许可证的请求重放到 B。

配置步骤（后台 **设置 → Polaris 授权服务** 按同样三步引导）：

1. 填写 polaris-license 地址，点「生成」得到一个随机 API 密钥，保存。
2. 把页面上的配置复制到 polaris-license 的环境变量并重启：
   ```dotenv
   KAKU_API_SECRET=同一个密钥
   KAKU_HOOK_URL=https://shop.example.com/hooks/polaris
   KAKU_RENEW_URL=https://shop.example.com/renew?lid={license_id}
   KAKU_RENEW_BOT=https://t.me/YourShopBot?start=renew_{license_id}
   KAKU_API_ALLOW_IPS=Kaku 服务器的出口 IP（可选）
   ```
3. 点「测试连接」：检查地址、密钥、IP 白名单和两台服务器的时间，失败时直接提示该改哪里；成功时显示授权服务的套餐、功能和模板数量。

配置商品：

- **一键创建**：商品页「从授权模板创建」，勾选 polaris-license 里的许可证模板（如「专业版 · 年付」「企业版 · 年付」），每个模板成为一个规格；填好价格后上架即可。
- **按模板**：规格的发货方式选「Polaris 激活码」后选「按模板」，套餐、限额、功能和有效期都以 polaris-license 的模板为准；以后在授权服务修改模板，新售出的许可证随之更新，续费按模板的月数延长。
- **自定义**：选套餐、有效期 / 更新期（可快速选 1 个月 … 永久）、节点 / 用户 / 插件上限，勾选高级功能或「全部高级功能（含以后新增）」。功能名称来自授权服务，保存时校验，不会因为拼写错误导致付款后发货失败。
- **JSON**：仍可直接编辑原始配置（同样会校验）。

「可用于续费」的规格出现在同套餐许可证的续费页；永久和离线许可证不能续费。离线许可证发货为 .lic 文件，无法吊销。后台「授权」页可以手动续期，默认同时延长更新期。

需要 polaris-license 包含 `/admin/v1/catalog`（2026-10 之后的版本）才能使用模板和功能列表；旧版本仍可用「自定义」，功能列表使用内置值。

polaris-license 还没有部署管理接口时，可以先在授权后台批量生成激活码，当作普通卡密导入「卡密库存」规格（续费 / 退款吊销需人工）。

续费入口：面板「许可证」页的续费按钮打开 `/renew?lid=…`，买家用购买邮箱验证后下单；Telegram 里打开 `?start=renew_…` 深链接，原购买账号直接续费，其他账号需先回复购买邮箱。

## 卖面板用户套餐（Polaris 面板兑换码）

机场主可以用 Kaku 卖自己 Polaris 面板里的用户套餐和流量包：买家付款后，Kaku 调用面板接口生成兑换码并发货，买家在面板用户商店或面板 Telegram 机器人里兑换。

配置步骤：

1. 面板「扩展 → API 令牌」创建令牌，权限勾选「生成和作废兑换码」（`redeem:write`）。该权限只能读取套餐 / 流量包、生成兑换码、按码作废未使用的兑换码，不能读用户或改设置。需要面板包含此权限（2026-10 之后的版本）。
2. Kaku 后台 **设置 → Polaris 面板**：填写面板地址（包含后台路径，如 `https://panel.example.com/你的后台路径`）和令牌，保存后点「测试连接」，会显示令牌名称和套餐、流量包数量。
3. 新增规格，发货方式选「Polaris 面板套餐兑换码」，在列表里选一个套餐或流量包，可设置兑换码有效期（0 = 不过期）。

| 面板接口 | 用途 |
| --- | --- |
| `GET {后台}/api/v1/plans`、`/traffic-packs` | 规格页列出可卖的套餐 / 流量包 |
| `POST {后台}/api/v1/redeem-codes` | 发货：生成兑换码，批次名为订单号 |
| `GET {后台}/api/v1/redeem-codes?batch=订单号` | 重试发货时复用已生成的码，不会重复生成 |
| `DELETE {后台}/api/v1/redeem-codes` | 退款：作废这笔订单中未兑换的码；已兑换的码保留并在退款时提示 |

兑换说明（设置页「兑换方法」）会显示在订单页、发货邮件和 Telegram 消息里。

## Telegram 机器人

1. 在 @BotFather 创建机器人，把 Token 填到 **设置 → Telegram Bot**，保存。
2. 站点地址必须是 HTTPS，点击 **设置 Webhook**。
3. 需要 Stars 支付时，在 **设置 → 支付** 勾选 Telegram Stars，并给规格填 Stars 价格。

买家命令：`/start`、`/shop`（内联键盘选购）、`/orders`（最近订单，可重新查看卡密）、`/renew lic_xxx`。管理员（设置里的 Chat ID）会收到新订单、发货失败、库存不足、新工单推送，并可用 `/stats` 看今日销售。

> 注意：Telegram 要求在机器人内销售数字商品使用 Stars。是否在机器人里提供易支付 / USDT 链接请自行评估平台政策。

## 与设计文档的差异

| 设计 | 实现 | 原因 |
| --- | --- | --- |
| React + Vite 前后台 | Go 模板服务端渲染 + 少量原生 JS | 单二进制、无构建链；所有页面无 JS 也能用 |
| Redis + asynq 异步任务 | 数据库任务表（`jobs`）+ 进程内轮询 | 少一个依赖，重启不丢任务；1C1G 可跑 |
| Redis 限流 / 会话 | 进程内限流与 Bot 会话 | 单实例部署足够；多实例需改回 Redis |
| 可选 Stripe | 未实现 | 二期 |
| HMAC 签 `ts + "." + body` | 额外签入方法和路径 | 防跨许可证重放 |

## 开发

```bash
go test -race ./...
go vet ./...
```

## 发布

每次合并到 `main` 自动发布：版本号在最新 tag 上递增补丁号，跑完测试后构建各系统二进制发布到 GitHub Release，并推送多架构镜像到 GHCR。提交信息带 `[skip release]` 可跳过；也可在 Actions 里手动运行 release 选择 minor / major，或直接推送 `v*` tag。

代码结构：

```
cmd/kaku          入口（serve / keygen / reset-admin）
internal/model    数据表
internal/store    数据库、设置（加密字段）、审计日志、持久任务队列
internal/secure   AES-GCM、HMAC、Argon2id
internal/payment  易支付、USDT-TRC20
internal/deliver  发货器：stock / text / manual / webhook / polaris
internal/polaris  polaris-license 管理接口客户端与回调验签
internal/shop     下单、定价、支付确认、发货重试、退款、续费
internal/tgbot    Telegram 机器人
internal/web      网页商店、支付回调、后台
```
