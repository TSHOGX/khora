# Sandbox Engine — API 与 CLI 契约(§6)

> 属 [SPEC.md](./SPEC.md);全局 §编号跨文件连续,导航见 SPEC.md 顶部「文档地图」。

---

## 6. API 与 CLI 契约

后端只暴露 **sandbox-centric** 的 API,**全 ID 寻址**;产品概念(session / mode 等)不进
后端。**CLI 与前端是同一套 API 的平级 client**,CLI 是一等公民,不是后门。

### 6.1 资源与端点(REST 形态;与 Connect-RPC 服务的投影待对齐,见 open-decisions §5.E 行)

```text
# Sandbox 生命周期
POST   /v1/sandboxes                 {template_id, network?, volumes?, guest_user?}  → {sandbox_id}
GET    /v1/sandboxes/{id}                                                          → {state, ...}
POST   /v1/sandboxes/{id}/pause
POST   /v1/sandboxes/{id}/resume
DELETE /v1/sandboxes/{id}                                                          (destroy)
GET    /v1/sandboxes                                                               (list)

# Process(连接)— 起进程 / 重连 / 输入 / 信号 / 列进程;PTY 是带 pty 字段的特例,pid 寻址
POST   /v1/sandboxes/{id}/processes  {cmd, args?, pty?, cwd?, env?, guest_user?}   → {pid, stream}
POST   /v1/sandboxes/{id}/processes/{pid}/connect                                  → {stream}  (重连)
POST   /v1/sandboxes/{id}/processes/{pid}/signal  {signal}
GET    /v1/sandboxes/{id}/processes                                                (实时 list,无注册表)
# stream 走双向通道(Connect-RPC / HTTP-2 stream),承载 stdin/stdout/stderr 与 pty 输入

# FS(读写沙箱文件)— read 可带 offset,无 watch;大文件走旁路
GET    /v1/sandboxes/{id}/fs?path=&offset=&len=                                    (read / stat)
PUT    /v1/sandboxes/{id}/fs?path=                                                 (write)
POST   /v1/sandboxes/{id}/fs/list     {path}
# agent 会话(session)不在 engine API 内——它是 engine 外 adapter 在 process/fs 上的加法(§5.E)

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

### 6.2 设计约束

- **ID 寻址**:对外只认 sandbox / template / volume 的不透明 ID 与进程 pid,不暴露宿主路径或
  placement。同一要求贯彻到引擎内部的 Overlay 工件(§3.1.1)。
- **创建期注入身份**:网络 / Volume 挂载 / guest 默认身份(create 参数 `guest_user`,即 §2.2 的 guest
  默认运行用户)在 create 时一次性绑定(不做晚绑定);但身份**经控制面通道注入**这一**流程形状**不被
  抄近路绕过,以后接预热池(晚绑定)复用同一注入点即可。process 上的 `guest_user` 是另一回事——它只是
  该次 `Start` 的 run-as(以哪个 guest 用户 exec,类比 `sudo -u`),不改沙箱创建期绑定的默认身份。
- **store / runtime provider 走接口**:换后端 / 加 provider 是注册一个新实现,不改上层模型
  (§1.3 只做加法;provider 始终是抽象契约,见 §1.1 / §5.A)。
