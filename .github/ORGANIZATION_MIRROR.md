# PolarisPanelOrg 自动镜像

个人仓库 `cuijianzhuang/<同名仓库>` 为源，`PolarisPanelOrg/<同名仓库>` 为单向镜像。此工作流仅允许同步本仓库对应的同名目标。

## 同步内容

- 同步全部分支、标签和 Git 历史。普通推进采用原子快进推送；组织端分叉或标签被改写时明确失败，等待人工确认，不强制覆盖或删除。
- 公开／私有状态跟随源仓库；状态修改失败时停止复制，避免把私有内容写入公开仓库。组织端关闭 Actions，只接收代码和成品，不重复运行发行构建。
- 初次只复制最新正式版和最新预发布，包括版本说明、正式／预发布／草稿状态及所有二进制附件。保留源正文，另附不可见来源标记，用于安全重试和识别镜像管理的附件。
- 后续新发布版本、已经镜像的旧版本更新、手工触发的特定版本，以及创建镜像后被重新上传附件的旧版本都会继续同步。源码 ZIP／TAR 由目标标签自动生成。
- 附件按大小和 SHA-256 校验，文件逐个处理和清理临时副本。所有附件确认齐全后才发布镜像 Release；失败留在草稿，下次继续核对。重复运行跳过已经一致的文件。
- Git push、Release 事件、发行／CI 工作流结束触发同步，每小时兜底。GitHub Actions 有排队时间，属于事件触发的近实时同步，不保证秒级。`GITHUB_TOKEN` 创建的 Release 不会再次触发 `release` 事件，因此同时监听发行工作流完成。

## 需要配置的凭据

在每个**源仓库**的 `Settings → Secrets and variables → Actions → Repository secrets` 中分别添加同名的 `MIRROR_TOKEN`。

推荐为每个镜像建立一个独立的 fine-grained PAT：

- Resource owner：`PolarisPanelOrg`。
- Repository access：仅选择对应的同名组织仓库。
- Repository permissions：`Contents: Read and write`、`Workflows: Read and write`、`Administration: Read and write`。Contents 用于推送和 Release；Workflows 用于复制工作流文件；Administration 用于维持公开／私有状态、默认分支和关闭组织端重复构建。
- 源仓库读取使用它自己的短期 `GITHUB_TOKEN`，无需给上述 PAT 授权个人账号的私有仓库。

Token 到期后需在源仓库更新 Secret。缺少凭据会明确失败，不再显示“成功但跳过”。凭据不写进 Git remote、脚本、Release 正文或日志；下载附件跳转到 GitHub 资源域名时移除 API Authorization。

首次手动运行 `mirror`，`history` 保持 `latest`。选择 `all` 才会导入全部历史 Release 附件（应先确认历史附件体积并预留足够运行时间）。

## 边界

Issues、PR、星标、讨论、GitHub Packages／GHCR 镜像和 Actions 历史不属于 Git／Release 镜像。GitHub 上的下载次数和原作者发布账户也不会变成相同记录。保留个人仓库作为写入入口；组织端用于分发和备份。

删去源 Release 不会自动删除组织端已发布的历史副本；Git 分支或标签删除、强制改写也留待人工确认。发布历史不会因为一次 API 列举失败而被清空。

测试：`python3 -m unittest discover -s .github/scripts -p 'test_mirror_sync.py' -v`；工作流另用 `actionlint` 验证。
