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
#   资源上限三轴见 §3.4(Binding);按轴条件必填的规则见 §6.2。准入不足 → 507。
GET    /v1/sandboxes/{id}                                                    → {sandbox_id, state}
#   只读生命周期态。死因已落库(§4.3)但**不在本读面上**,登记见 open-decisions §4。
PATCH  /v1/sandboxes/{id}            {cpu_millis?, memory_byte?, disk_byte?, idle_timeout?, retention?}
                                                          → {sandbox_id, state, applies_at}
#   改 Binding,生效点与可改状态见 §3.4。**wire 语义与 create 不同**:这里"未给"与"给 0"必须可区分
#   (帽的 0 = 无上限、`retention: 0` = 不驱逐),故请求字段**可空**——未给 = 不动该字段。TTL 收
#   duration 字符串("48h")。`applies_at` ∈ {"now"(仅改 TTL), "next-resume"(改了任一资源上限)}。
#   对 `running` 改帽 → 409「先 pause」;抬上限重过准入门(可 507 / 400)。不发 §4.1 事件(非状态转换)。
POST   /v1/sandboxes/{id}/pause
POST   /v1/sandboxes/{id}/resume
#   resource-terminated 的沙箱拒绝 resume → 409(§4.3;先 PATCH 抬帽)
DELETE /v1/sandboxes/{id}                                                          (destroy)
GET    /v1/sandboxes                                                               (list)

# Template(不可变工件:opaque ID 作身份,存在即状态,§3.1 / §4.3)
GET    /v1/templates
POST   /v1/templates                 {from_sandbox_id}   (Promote: 静止后固化,源沙箱留 `paused`,§3.1) → {template_id}
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
client 从 `transport.proto` 经 `buf` 生成 stub、经 `orchestrator` 反代到 `sandboxd`(D6.1)。**沙箱寻址
走带外请求头 `Engine-Sandbox-Id`**:proto 本身只按 guest-native pid 寻址(D4.6 词汇边界),反代 strip
掉该头、在引擎内解析当前端点(placement 永不外泄,§6.2 / D6.4)。下方 REST 行**仅作形状示意**(说明
有哪些操作、如何 pid/path 寻址),**不是可实现的权威端点**——真正的 wire 看 proto:

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
  先存在而绑定未定),故不放松本约束。**guest 身份不在其列**:默认 guest root,引擎不建模 guest 用户,
  create 与 `Start` 都无 run-as 参数(§2.2 / §3.2.1)。
- **资源上限字段按轴条件必填**:某轴被节点预算计量时,create 必须在该轴给出取值(`0 = 无上限` 仅在
  未计量轴上合法);缺失即拒(400),这与"节点已满"(507)是两件不同的事。这是 §6.1 唯一取决于部署
  配置的字段语义,故显式写在此处而非留给实现。
- **状态码**:准入容量不足 → **507**(说的是"容量",而非 409 那种调用方可重试的状态冲突);
  `resource-terminated` 拒绝 resume → **409**;计量轴缺帽 / 请求体不合法(负的帽、无法解析的 duration)
  → **400**;被引用的 Template 删除 → **409**;源态不合法的动词(如对 `running` 改帽、对非 `paused`
  沙箱 resume)→ **409**;未知 sandbox / template ID → **404**。
- **store / runtime provider 走接口**:换后端 / 加 provider 是注册一个新实现,不改上层模型
  (§1.3 只做加法;provider 始终是抽象契约,见 §1.1 / §5.A)。
