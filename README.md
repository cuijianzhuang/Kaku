# Kaku

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
4. **商品 → 管理分类**，新增分类；**新增商品**，再为商品**新增规格**，选择发货方式。
5. 发货方式为「卡密库存」时，到 **卡密** 页粘贴或上传 TXT / CSV 导入。

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
| `POST /admin/v1/keys` | 新购：签发激活码（order_no 做幂等键；离线 SKU 返回 .lic 文本） |
| `POST /admin/v1/licenses/{id}/extend` | 续费：延长同一许可证，面板下次校验即生效，无需重新激活 |
| `POST /admin/v1/licenses/{id}/revoke` | 退款：吊销，面板下次校验回落社区版 |
| `GET /admin/v1/licenses/{id}` | 后台同步状态 |
| 回调 `POST {kaku}/hooks/polaris` | 激活 / 停用事件（可选） |

请求签名：`X-Kaku-Timestamp`（Unix 秒）+ `X-Kaku-Signature = hex(HMAC-SHA256(secret, ts + "." + METHOD + " " + PATH + "." + body))`，时间窗 5 分钟。签名里包含方法和路径，防止把针对 A 许可证的请求重放到 B。

配置步骤：

1. 生成一个 ≥32 位的随机密钥（如 `openssl rand -hex 32`）。
2. polaris-license 环境变量：
   ```dotenv
   KAKU_API_SECRET=上面的密钥
   KAKU_API_ALLOW_IPS=Kaku 服务器的出口 IP
   KAKU_HOOK_URL=https://shop.example.com/hooks/polaris
   KAKU_RENEW_URL=https://shop.example.com/renew?lid={license_id}
   KAKU_RENEW_BOT=https://t.me/YourShopBot?start=renew_{license_id}
   ```
3. Kaku 后台 **设置 → Polaris 授权服务**：填写 polaris-license 地址和同一个密钥。
4. 新增规格，发货方式选「Polaris 激活码」，配置示例：
   ```json
   {"plan": "pro", "days": 365, "updates_days": 365,
    "limits": {"nodes": 20, "users": 0, "plugins": 0},
    "features": ["*"], "offline": false, "max_activations": 1, "renewable": true}
   ```
   `renewable: true` 的规格会出现在续费页（同套餐的许可证）。`offline: true` 发货为离线 .lic 文件，无法吊销，只适合企业版。

polaris-license 还没有部署管理接口时，可以先在授权后台批量生成激活码，当作普通卡密导入「卡密库存」规格（续费 / 退款吊销需人工）。

续费入口：面板「许可证」页的续费按钮打开 `/renew?lid=…`，买家用购买邮箱验证后下单；Telegram 里打开 `?start=renew_…` 深链接，原购买账号直接续费，其他账号需先回复购买邮箱。

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
