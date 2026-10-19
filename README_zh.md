# gitrieve

[English](README.md) | 简体中文

Git Retrieve（gitrieve）是一个用于从任何Git服务器归档 Git 仓库的工具。

- [功能](#功能)
- [安装](#安装)
- [使用方法](#使用方法)
  - [repository](#repository)
  - [run](#run)
  - [daemon](#daemon)
  - [release](#release)
- [Web UI](#web-ui)
- [配置](#配置)
- [存储](#存储)
- [防删除同步](#防删除同步)
- [使用 Docker 运行](#使用-docker-运行)
  - [Docker CLI](#docker-cli)
  - [Docker Compose](#docker-compose)

## 功能

- 从任何Git服务器归档 Git 仓库
- 归档用户/组织的仓库（见 [配置](https://github.com/wnarutou/gitrieve/wiki/Configuration#repository))
- 定时任务
- 多种存储类型（见 [存储](#存储)）
- **防删除同步** —— 同步过程绝不会删除本地已拉取的代码与完整历史，即使上游仓库被下线、DMCA 禁用、删除、私有化或被替换为单个 README（见 [防删除同步](#防删除同步)）
- Docker 支持（见 [使用 Docker 运行](#使用-docker-运行)）

## 安装

```bash
curl -sSfL https://raw.githubusercontent.com/wnarutou/gitrieve/main/install.sh | sh -s -- -b /usr/local/bin
```

或从 [Release](https://github.com/wnarutou/gitrieve/releases) 获取。

## 使用方法

你需要创建一个配置文件来使用gitrieve。

```yaml
repository:
  - name: gitrieve
    url: github.com/wnarutou/gitrieve
    cron: "0 * * * *"
    storage:
      - localFile
      - backblaze
    useCache: True
    allBranches: True
    depth: 0
    downloadReleases: True
    downloadIssues: True
    downloadWiki: True
    downloadDiscussion: True

storage:
  - name: localFile
    type: file
    path: ./repo
  - name: backblaze
    type: s3
    endpoint: s3.us-west-000.backblazeb2.com
    region: us-west-000
    bucket: your-bucket-name
    accessKeyID: your-access-key-id
    secretAccessKey: your-secret-access-key
```

然后，你可以使用配置文件运行gitrieve。

```bash
gitrieve -c config.yaml
# 或者如果你的配置文件名为config.yaml，只需调用gitrieve
gitrieve
```

### repository

`repository`命令会归档在配置中定义的单个 Git 仓库。

```bash
gitrieve repository gitrieve
```

### run

`run`命令会归档在配置中定义的所有 Git 仓库。

```bash
gitrieve run
```

结合cron，你可以定期归档 Git 仓库。

### release

`release`命令会归档指定 Git 仓库的所有发布产物。

```bash
gitrieve release gitrieve
```

### daemon

`daemon`命令会启动一个守护进程，它会在后台运行，归档在配置中定义的所有 Git 仓库。

```bash
gitrieve daemon
# 使用 nohup 后台运行
nohup gitrieve daemon &
```

## Web UI

`server` 命令会启动一个 Web UI 和 HTTP API，让你可以通过浏览器管理归档任务和配置。它也会执行仓库的 Cron 调度，因此无需再单独运行 `daemon` 进程。

```bash
gitrieve server
# 默认监听 http://localhost:8080
```

在 UI 中你可以触发归档任务、查看实时日志，并直接编辑仓库/存储配置而无需手动修改 `config.yaml`。仓库编辑、配置导入或配置重载后，Cron 调度会立即刷新。服务器通过 `config.yaml` 中可选的 `server` 配置段进行配置（host、port 以及可选的 Bearer Token 认证）。

详见 [Web UI 指南](docs/web-ui.md) 和 [API 文档](docs/api.md)。

Repositories 页面提供服务端全局健康计数、失败/从未同步/逾期/卡住筛选、
优先关注排序、组件详情和安全的批量重试。页面会区分最近一次尝试和最近一次完整
成功的备份；最新执行以 `pending` 或 `running` 状态超过 `syncStuckThreshold`
都会判定为卡住。不支持的已启用组件记为 `skipped`，真实组件失败会使整个执行失败。
该页面**绝不自动刷新**，不会轮询、设置定时器，也不会因任务完成 SSE 事件而重载；
需要新快照时请点击 Refresh 或重新进入页面。

## 配置

Issue、Discussion 和 Release 使用的 GitHub API 请求会在每个 gitrieve 进程内统一协调。可选配置及默认值如下：

```yaml
githubApiConcurrency: 2
githubMinRequestInterval: 200ms
githubLowRemainingThreshold: 100
githubScheduleJitter: 30s
syncOverdueGrace: 30m
syncStuckThreshold: 24h
cocurrencyNum: 6
```

`syncOverdueGrace` 和 `syncStuckThreshold` 的默认值分别为 `30m` 和 `24h`；
卡住阈值同时适用于 `pending` 和 `running`，非正值会恢复为默认值。沿用已有拼写的
`cocurrencyNum` 会统一限制 server 的
Cron、手动执行和批量重试实际并发数；超出上限的任务保持 `pending`，同一仓库的
重复活动任务会被拒绝。服务重启时，上一进程遗留的 `pending`/`running` 执行及
组件记录会被对账为 `failed`，不会永久显示为活动状态。

设置 `githubScheduleJitter: 0s` 可关闭错峰。错峰仅应用于 daemon 的 cron 任务，手工命令和 Web API 任务仍会立即开始。这些配置用于降低请求突发，不会增加 GitHub 配额，也不会协调不同进程或主机。

有关配置，你可以查看此[示例](config/example.config.yaml)。

更多细节，可查看[配置文档](https://github.com/wnarutou/gitrieve/wiki/Configuration)。

## 存储

gitrieve支持多种存储类型。

- [x] 文件
- [x] AWS S3

## 防删除同步

gitrieve 的一个核心设计目标是：**一旦代码和历史被拉取到本地，同步过程绝不删除它们** —— 即使上游仓库被下线、DMCA 禁用、删除、私有化或被替换为单个 README。这使得 gitrieve 适合作为真正的归档/备份工具，而不仅仅是镜像。

该特性由同步逻辑在以下几方面保证：

- **远端不可达 → 提前退出，什么都不动。** 当上游仓库被删除、被禁用（如 DMCA）或被私有化且无权访问时，`fetch` 会失败，同步随即提前返回。不会执行归档，也不会执行清理，本地缓存以及此前已归档的快照均原封不动。
- **分支只增不删。** 同步遍历远端分支来创建/更新本地分支，但从不删除本地分支。上游已删除的分支在本地依然保留。
- **使用 pull 而非 reset。** 更新通过 `git pull`（合并）应用，从不使用 `git reset --hard origin`。即便上游通过 force-push 重写了默认分支，也只会移动 `origin/*` 追踪引用；你的本地分支及其旧提交对象不会被覆盖或丢弃。
- **旧提交被保留。** 由于提交是不可变对象，且同步从不强制移动本地引用，已经拉取的完整历史会留在本地 `.git` 对象库中。任一历史提交都可通过 `git checkout <旧hash>` 恢复。

为获得最大可恢复性，建议配置：

- `allBranches: true` —— 确保每个分支的提交都被拉入本地对象库。
- `useCache: true` —— 跨同步保留本地缓存目录（含 `.git`）作为额外的磁盘安全网（若不开启，工作目录会在每次同步结束时被删除）。

需要注意的一点：归档写入固定文件名（如 `repo.tar.gz`），会覆盖同路径下的旧归档。因此，若上游仍可访问但其默认分支被重写为单个 README，*新*快照会替换该路径下此前正常的归档。本地缓存的代码与历史仍然安全（见上文），但若要保留不同的历史快照，建议在对象存储（S3/B2）上启用版本控制，或以带版本号的路径保存归档。

## 使用 Docker 运行

### Docker CLI

一次性运行。 
- 修改 `${pwd}/config/example.config.yaml` 为你的配置文件本地路径。
- 自定义 `${pwd}/repo:/repo` 为你需要的存储路径。容器内路径需要与配置文件中的路径一致。

```bash
docker run --rm \
    -v ${pwd}/config/example.config.yaml:/config.yaml \
    -v ${pwd}/repo:/repo \
    wnarutou/gitrieve:latest \
    run
```

### Docker Compose

示例Compose配置，见 [docker-compose.yml](docker-compose.yml)。

```bash
git clone https://github.com/wnarutou/gitrieve.git
docker compose up -d
```

发布的镜像是多架构 manifest，覆盖 `linux/amd64` 和 `linux/arm64`，在 x86_64 和 Apple Silicon 机器上均可使用。

### 升级前进入维护模式

在 Web 页面顶部点击“进入维护模式”，等待提示“可以安全升级”后，再更新容器：

```bash
docker compose pull
docker compose up -d
```

维护期间禁止提交新任务，排队任务取消；正在执行的 Git/Wiki 拉取会停止，Issue/Discussion 完成当前列表页及其评论、回复，并将已下载内容打包写入存储后停止。Release 完成当前文件下载及存储后停止。已经开始的归档写入会等待完成，实际下载或存储错误仍记为失败。

关闭 `useCache` 时，尚未下载完整列表的 Issue/Discussion 会另存为 `issues.partial.<ID>.tar.gz` / `discussions.partial.<ID>.tar.gz`，保留之前的完整归档；这些部分归档不会自动删除。下一次完整同步照常更新正式归档。启用缓存时，后续成功同步会重新发布缓存归档，即使远端没有新变化，以补偿之前中断或失败的写入；这会增加打包和上传开销。

维护状态只保存在内存中。升级重启后恢复正常定时调度，不恢复旧队列，也不立即重跑中止的任务；下一次 cron 触发或手动操作会创建新任务。没有配置 cron 的仓库需手动触发。不重启时，可在收尾完成后点击“退出维护模式”重新开放调度。

`server` 模式收到 SIGTERM/SIGINT 时也会执行相同收尾。Compose 示例提供 `stop_grace_period: 10m`，超时后 Docker 仍可能强制结束容器，因此大文件或评论很多时应先在页面等待维护完成。数据库状态写入失败时，页面不会提示可以安全升级，请先查看日志并处理存储问题。

维护 API（沿用现有认证）：`GET /api/maintenance` 查询状态，`POST /api/maintenance` 进入维护，`DELETE /api/maintenance` 退出维护。`ready: true` 表示任务已收尾且状态保存成功；退出维护不会自动提交任务。维护状态刷新不会自动刷新仓库列表。
