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
POST   /v1/sandboxes                 {template_id, network?, volumes?, idle_timeout?, retention?,
                                      cpu_millis?, memory_byte?, disk_byte?}        → {sandbox_id}
#   资源上限三轴见 §3.4(Binding)。`0 = 无上限` 仅在该轴未被节点预算计量时合法;被计量则该轴
#   必填,缺失即 400(与"节点已满"507 不同)。准入不足 → 507。
GET    /v1/sandboxes/{id}                                                          → {state, ...}
PATCH  /v1/sandboxes/{id}            {cpu_millis?, memory_byte?, disk_byte?, idle_timeout?, retention?}
                                                          → {sandbox_id, state, applies_at}
#   改 Binding(§3.4)。**字段语义与 create 不同**:这里"未给"与"给 0"必须可区分,因为 0 本身有意义
#   (帽的 0 = 无上限、`retention: 0` = 不驱逐),故请求字段是**可空**的——未给 = 不动该字段,给 0 = 就
#   把它设成 0。TTL 仍收 duration 字符串("48h")。
#   资源上限仅 `paused` 可改(下次 resume 生效,applies_at="next-resume"),对 `running` 改帽 → 409
#   "先 pause";TTL 两态皆可改、立即生效(applies_at="now")。抬上限重过准入门(可 507 / 400);
#   抬 memory_byte 在**同一提交**清死因,这是 §4.3 改帽转机能走通的必要一步。不发 §4.1 事件(不是状态转换)。
POST   /v1/sandboxes/{id}/pause
POST   /v1/sandboxes/{id}/resume
#   resource-terminated 的沙箱拒绝 resume → 409(§4.3;先 PATCH 抬帽)
DELETE /v1/sandboxes/{id}                                                          (destroy)
GET    /v1/sandboxes                                                               (list)

# Template(不可变工件:opaque ID 作身份,存在即状态,§3.1 / §4.3)
GET    /v1/templates
POST   /v1/templates                 {from_sandbox_id}   (Promote: 静止后固化沙箱,§3.1)        → {template_id}
DELETE /v1/templates/{id}            (被沙箱/子层引用时 → 409 template in use;引用-回收原子性 §4.3)

# Volume(与沙箱生命周期解耦)
POST   /v1/volumes                   {size?, backend?, access_mode?}                → {volume_id}
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
(示意)  processes                     {cmd, args?, pty?, cwd?, env?}   → {pid, stream}
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
- **创建期注入绑定,改绑定走同一注入点**:网络 / Volume 挂载在 create 时一次性绑定(不做晚绑定);
  但**经控制面通道注入**这一**流程形状**不被抄近路绕过,以后接预热池(晚绑定)复用同一注入点即可。
  **改**绑定经 `PATCH /v1/sandboxes/{id}`,一个沙箱**任何时刻都持有一份完整绑定**——`PATCH` 只把一份
  完整绑定换成另一份完整绑定(未给的字段不变,**没有"待定/未解析"值**),物化仍在 placement 时经同一条
  控制面注入点发生。这是**改绑定(rebinding)**,不是 §1.4 预热池那个意义上的**晚绑定**(晚绑定 = 沙箱
  先存在而绑定未定)。故 `PATCH` 不放松本约束——它恰好把 §1.4 承诺"预热池将来复用"的那个注入点**先建了
  出来**。**guest 身份不在其列**:默认 guest root,引擎不建模 guest 用户,create 与 `Start` 都无 run-as
  参数(§2.2 / §3.2.1)。
- **资源上限字段按轴条件必填**:某轴被节点预算计量时,create 必须在该轴给出取值(`0 = 无上限` 仅在
  未计量轴上合法);缺失即拒(400),这与"节点已满"(507)是两件不同的事。这是 §6.1 唯一取决于部署
  配置的字段语义,故显式写在此处而非留给实现。
- **状态码**:准入容量不足 → **507**(说的是"容量",而非 409 那种调用方可重试的状态冲突);
  `resource-terminated` 拒绝 resume → **409**;计量轴缺帽 → **400**;被引用的 Template 删除 → **409**。
- **store / runtime provider 走接口**:换后端 / 加 provider 是注册一个新实现,不改上层模型
  (§1.3 只做加法;provider 始终是抽象契约,见 §1.1 / §5.A)。
