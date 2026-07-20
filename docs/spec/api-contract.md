# Sandbox Engine — API 与 CLI 契约(§6)

> 属 [SPEC.md](./SPEC.md);全局 §编号跨文件连续,导航见 SPEC.md 顶部「文档地图」。

---

## 6. API 与 CLI 契约

后端只暴露 **sandbox-centric** 的 API,**全 ID 寻址**;产品概念(session / mode 等)不进
后端。**CLI 与前端是同一套 API 的平级 client**,CLI 是一等公民,不是后门。

### 6.1 资源与端点(控制面 REST + 数据面 Connect-RPC,分工见 [`deep-dives/transport.md`](./deep-dives/transport.md) D6)

> **外沿协议分工**:沙箱/template/volume/network **生命周期 = REST + JSON**(控制面块,权威端点,
> OpenAPI 事后描述);**process / fs 数据面 = Connect-RPC**(`engine.transport.v1` 两服务,权威契约
> 是 `transport.proto`,client 经 `buf` 生成 stub)。client 一律经 `orchestrator` 居中、不直连
> `sandboxd`(§4 mediator / D6.1)。数据面块的 REST 行**仅作形状示意**,不是权威端点(D6.2)。

**控制面(REST + JSON,权威端点)** —— 沙箱 / template / volume / network 生命周期,全是低频
CRUD、无流:

```text
# Sandbox 生命周期
POST   /v1/sandboxes                 {template_id, network?, volumes?, guest_user?}  → {sandbox_id}
GET    /v1/sandboxes/{id}                                                          → {state, ...}
POST   /v1/sandboxes/{id}/pause
POST   /v1/sandboxes/{id}/resume
DELETE /v1/sandboxes/{id}                                                          (destroy)
GET    /v1/sandboxes                                                               (list)

# Template
GET    /v1/templates
POST   /v1/templates                 {from_sandbox_id}   (Promote: 静止后固化沙箱,§3.1)        → {template_id}
DELETE /v1/templates/{id}

# Volume(与沙箱生命周期解耦)
POST   /v1/volumes                   {size?, backend?}                              → {volume_id}
GET    /v1/volumes                                                                 (list)
GET    /v1/volumes/{id}                                                            (inspect)
DELETE /v1/volumes/{id}
POST   /v1/volumes/{id}/snapshot                                                   → {volume_id}
POST   /v1/volumes/{id}/clone                                                      → {volume_id}
POST   /v1/volumes/{id}/import       (直连写入;无 live-rw 挂载时才放行,§5.C 写入 regime)
GET    /v1/volumes/{id}/export       (直连读出)

# Network(策略在 create 时绑定,改策略走 resume 边界;publish 是显式入站例外,可热加)
GET    /v1/sandboxes/{id}/network
POST   /v1/sandboxes/{id}/publish    {port, auth?}                                 → {url}
```

**数据面(Connect-RPC,权威 wire 是 proto,非下方 REST 行)** —— process / fs 有 server-stream +
强类型帧,**权威契约是 `engine.transport.v1` 的 `ProcessService` / `FsService`**(D4.3/D4.4),
client 从 `transport.proto` 经 `buf` 生成 stub、经 `orchestrator` 反代到 `sandboxd`(D6.1)。下方
REST 行**仅作形状示意**(说明有哪些操作、如何 pid/path 寻址),**不是可实现的权威端点**——真正的
wire 看 proto:

```text
# Process(连接)— 起进程 / 重连 / 输入 / 信号 / 列进程;PTY 是带 pty 字段的特例,pid 寻址
#   权威:ProcessService.{Start,Connect,StreamInput,SendInput,SendSignal,Update,List,CloseStdin}
(示意)  processes                     {cmd, args?, pty?, cwd?, env?, guest_user?}   → {pid, stream}
(示意)  processes/{pid}/connect                                                     → {stream}(重连,先回放有界环形缓冲再接 live,D4.3)
(示意)  processes/{pid}/signal        {signal}
(示意)  processes                                                                    (实时 list,无注册表)

# FS(读写沙箱文件)— read 可带 offset,无 watch;大文件走 multipart 旁路
#   权威:FsService.{Read,Write,List,Stat,MakeDir,Remove,Move}
(示意)  fs?path=&offset=&len=                                                        (read / stat)
(示意)  fs?path=                                                                     (write)
(示意)  fs/list                       {path}
# agent 会话(session)不进 engine API——engine 外 adapter 在 process/fs 上的加法(§5.E / D1.2)
```

### 6.2 设计约束

- **ID 寻址**:对外只认 sandbox / template / volume 的不透明 ID 与进程 pid,不暴露宿主路径或
  placement。同一要求贯彻到引擎内部的 Overlay 工件(§3.1.1)。
- **创建期注入身份**:网络 / Volume 挂载 / guest 默认身份(create 参数 `guest_user`,即 §2.2 的 guest
  默认运行用户)在 create 时一次性绑定(不做晚绑定);但身份**经控制面通道注入**这一**流程形状**不被
  抄近路绕过,以后接预热池(晚绑定)复用同一注入点即可。process 上的 `guest_user` 是另一回事——它只是
  该次 `Start` 的 run-as(以哪个 guest 用户 exec,类比 `sudo -u`),不改沙箱创建期绑定的默认身份。
- **store / runtime provider 走接口**:换后端 / 加 provider 是注册一个新实现,不改上层模型
  (§1.3 只做加法;provider 始终是抽象契约,见 §1.1 / §5.A)。
