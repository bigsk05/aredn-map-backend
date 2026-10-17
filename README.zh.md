# aredn-map-backend（中文）

扫描 AREDN mesh 并把结果供出去。从一个或几个种子出发读每个节点自己的页面，记录页面上
公布的无线链路与邻居表，用 ICMP 探测可达性，结果全部放在内存里。

没有数据库。存储只在进程里，重启即回到种子重新开始；每轮扫描都会重读已知内容，因此消失
的节点不再被上报。

## 部署

本服务只在自己的域名上应答，完全不知道前端在哪。把前端仓库（`aredn-map-frontend`）
作为纯静态站点放在另一个域名上，构建时用 `NUXT_PUBLIC_API_BASE` 指过来，并把那个前端域名
填进 `server.allow_origins`。`allow_origins` 留空即回 `Access-Control-Allow-Origin: *`，
浏览器从任何地方读这个 API 都可以——请求不带 cookie 也不带会话。

`server.static_dir` 只是给"一台机器一个进程"留的捷径，默认为空，也就是只出 JSON 和 socket。

## 运行

```sh
cp config.example.yaml config.yaml   # 地图密钥只写进 config.yaml
go build -o bin/aredn-map .
./bin/aredn-map                    # 从 ./ 或 ../ 读 config.yaml
./bin/aredn-map -config /path/to/config.yaml
./bin/aredn-map -once              # 只跑一轮完整扫描，打印摘要后退出
```

`AREDNSCAN_CONFIG` 也可以用环境变量指定文件。配置文件不存在不算错误：
`internal/infra/config/model.go` 里的默认值本身就是一台可用的扫描器，种子指向
`localnode.local.mesh`。

`config.yaml` 被 .gitignore 故意排除，仓库里的 `config.example.yaml` 的 `map` 密钥一律为空——
`TestLoadRepoConfig` 读的就是这份样本，一旦有密钥落进去测试就会失败。没有密钥时 API 照常工作，
只有底图会自报"未配置地图密钥"。

## 配置

关键项见英文 README 的表格，含义一致。两点值得注意：

- `scan.absorb_mesh_hosts`：把 supernode 主机表里的名字登记进来，但不扫描它们。
- `scan.revisit_after`：探测失败的主机进回访队列的等待时间，`0` 关闭这条队列。

## 结构

`main.go` 只做装配：配置、日志、资源、定时、HTTP。`internal/` 下 `infra/{config,logger}`
是基础设施，`model` 是数据契约，`coord` 是坐标换算，`repository/{aredn,ping,store}` 是数据
来源与内存表，`service/scanner` 是扫描引擎，`cron` 是定时器，`controller` 是按领域划分的
请求处理，`handler` 负责 Fiber 装配、路由表、WebSocket 推送与静态资源。

## 接口

`/api/health`、`/api/status`、`/api/nodes`、`/api/links`、`/api/topology`、
`/api/lookup?ip=`、`/api/config`、`/api/ws`（推送：每次变更一帧，只带上一帧之后被写入的记录），
以及 `POST /api/scan/{enable,run,incremental,stop}` 供运维使用。

## 一轮扫描怎么走

1. 读种子的状态页，它本地的连接关系成为第一层前沿。
2. 读每个节点的邻居表，出现的邻居同时成为节点与链路。
3. 用 ICMP 探测前沿。没有回应的先登记，到点再回访，不直接判成离线。
4. 抓每个节点的页面，取硬件、固件、坐标与发布的服务。
5. 失败的记下原因并进回访队列。

只出现在别人主机表里的名字会被登记，让 mesh 看起来完整，但不进入扫描队列：它们的页面从未
被抓取，所以显示成"未扫描"，而不是"未知电台"或"离线主机"。
