# M1 — 单沙箱生命周期(create / pause / resume / destroy 端到端跑通)

> **这份文档是什么。** engine 的**当前落地态计划**——不是架构规格。架构规格是
> `docs/spec/`(§1–§6,终态,跨文件全局编号)。本文只回答一件事:**M1 这一刀切到哪、
> 怎么算通过**。凡本文与 `docs/spec/` 冲突,以 `docs/spec/` 为准;本文只做「当前实现到哪一步」的
> 快照,随里程碑推进而重写。
>
> **§ 引用** = `docs/spec/` 的全局 spec 章节(§3.3、§5.E.1…);**D 引用** = 对应 deep-dive 的
> 小节(D5、D4.4…)。两套编号不混。

---

## 0. M1 的唯一目标:一条竖切打穿最难的五件事

M1 采用 **walking-skeleton 技术**(一次性阶段手法,非文档名):一条**最薄的端到端竖切**,单机、无 Volume / Network / Promote / 凭证。它存在的
意义不是「功能多」,而是**一刀同时触到引擎最难的五个关节**,把它们的接缝在真机上钉死:

```
Template(一个普通目录)
   │  ① §5.B Rootfs:外层内核 overlayfs 拼 lower(Template)/upper/work → merged
   ▼
冷启 runsc(--overlay2=none --file-access=shared;entrypoint = sandboxd init 模式当 PID1)
   │  ④ §5.A Runtime:provider 契约式拉起,产出 placement 句柄 + transport 端点
   ▼
sandboxd 经 /proc/self/exe re-exec 进 transport 模式 → netns 内 TCP listen
   │  ⑤ §5.E.1 guest 启动拓扑
   ▼
宿主经 netns veth IP 直连 → Health RPC 首次成功 → running
   │  ⑥⑦ §5.A placement 发现 IP + §4 探活门
   ▼
process.Start /bin/bash 流回 stdout  ·  fs.write 落一个文件
   │  §5.E:process / fs 两个无状态 RPC 服务
   ▼
pause(停容器、upper 留后端,零捕获)
   │  §4.2 FS-first,无内存快照
   ▼
resume(同一 upper 冷启新容器,换新 netns IP)
   │  §3.3:身份不编码 placement——换 IP 仍是同一沙箱
   ▼
fs.read 读回那个文件 → 字节一致  ✅ 竖切成立
```

**五个关节**:§5.A 起停 · §5.B overlay 组装 + pause/resume 零捕获 · §5.E transport + PID1/transport
分进程 · §4 mediator + 转换锁 · §3.3「换 IP 仍是同一沙箱」。这一刀过了,后续里程碑都是在已验证的
接缝上加厚,而不是在未验证的地基上叠功能。

---

## 1. M1 显式**不做**什么(边界,防 scope 蔓延)

按 `docs/spec/` 的里程碑阶梯,以下都**不在 M1**——不是遗漏,是刻意后置到已列明的里程碑:

| 不做 | 属于 | 为什么能安全后置 |
| --- | --- | --- |
| store 持久化 + 状态机三不变量(outbox / 转换锁 CAS / 崩溃对账) | M2(§4.4) | M1 用**进程内内存 store**先跑通竖切;§4.4 的并发/失败不变量是独立里程碑,要配故障注入测试 |
| reaper + 健康态四分(两层探活 × 连接活动) | M3(§4.3) | M1 只手动驱动 create/pause/resume/destroy,不引入自动回收 |
| Volume(挂载表 / 租约 / 直连写 regime) | M4(§5.C) | 竖切不挂任何 Volume;`volumes?` 参数存在但 M1 恒空 |
| Network egress 数据面 + deny-set + 凭证注入 | M5(§5.D / §2.4) | M1 用 `--network=none` 或最简 netns,**不出公网**;egress 安全是独立的安全关键里程碑 |
| Promote + 分层 Template + refcount GC | M6(§5.B / §4.3) | M1 的 Template 是一个静态目录,不 promote、不 fork |
| 凭证 store seam | M7(§5.D) | 无出网即无凭证注入 |
| Connect-RPC 完整 wire(multipart 旁路 / 环形回放 / keepalive) | 随 M1 长出，先最小 | M1 只需 process.Start / fs.read / fs.write / Health 四个 RPC 能跑;其余 D4 清单增量补 |

**M1 内存 store 是脚手架，不是设计**：M2 落 SQLite + §4.4-C 原子提交契约时它被替换，接口不变（store 是
§1.3 的 Behavior seam）。

---

## 2. M1 的接缝(现在就要画对，后续里程碑往里填)

三个 seam 是整个引擎的承重墙，M1 就要把**形状**立对（即便实现是最小的）：

- **runtime provider（§5.A 核心 seam）**：接口必须**契约式**，不泄漏宿主原语——
  `(rootfs 来源, 引流要求, 资源上限, guest 身份) → (placement 句柄, transport 端点, stop/start)`。
  M1 唯一实现 `local_runsc`。**禁止**在接口里出现「cgroup 路径 / netns fd / upper 绝对路径」这类
  只有本地进程给得出的东西（§5.A 的 ❌ 清单）——否则将来任何远端 provider 接不进来。
- **store（Behavior seam）**：`lifecycle-state` 的读 + 条件写（CAS）。M1 内存实现，M2 换 SQLite
  **不改接口**。M1 就要把 CAS 形状留出来（哪怕只有一个 goroutine），否则 M2 的转换锁无处落。
- **FS provider（§5.B seam）**：Overlay 对上层只暴露「ID + 引用的 Template + at-rest 可读」，
  **不暴露「它是一个宿主 upper 路径」**（§3.1.1 对内也 ID 寻址）。宿主路径锁死在 Rootfs 子系统内部。
  M1 唯一实现「留驻 upper 目录」（D3），content-addressed diff 层是 M6+ 演进项。

**一条铁律贯穿 M1（§4 开头）**：动词归 §4（mediator 持状态机、按序调子系统），机制归 §5，
**子系统之间永不互相调用**。M1 的 create/resume 就是 mediator 按 D5.1 的 ①→④→⑤→⑥→⑦ 依次调
Rootfs / Runtime / Transport，任何「Rootfs 去戳 Runtime」都是成环的起点，评审直接打回。

---

## 3. M1 冷启链（D5.1 在 M1 的裁剪版）

D5.1 的 canonical 链是 ①–⑦；M1 因为没有 Volume(②)和 Network(③)，裁剪成：

```
① §5.B Rootfs   ：create→建空 upper/work + 拼 merged overlay；resume→重挂同一 upper
   〔② Volume    ：M1 跳过——无挂载〕
   〔③ Network   ：M1 跳过——--network=none / 最简 netns，无 egress 数据面〕
④ §5.A Runtime  ：建 placement(netns/IP;M1 资源上限可空)→ 以 sandboxd(init 模式)为
                   entrypoint 拉起 runsc(--overlay2=none --file-access=shared)
⑤ guest 启动    ：PID1 装最小 reaper+信号 → re-exec 出 transport 模式子进程
                   → PR_SET_CHILD_SUBREAPER → bind netns TCP → listening
⑥ §5.A placement：发现容器当前 netns veth IP
⑦ §4 探活门     ：向 IP:port 发 Health RPC，轮询至首次成功 → state=running → API 返回
                   （M1 失败出口：resume→留 paused;create→清理半建 placement + 删记录）
```

**顺序/并发约束（M1 也守）**：① 必须在 ④ 之前（挂载在 runsc 启动时从 OCI spec 取，运行中不可
热改，§2.5）；⑦ 是「已拉起」与「running」之间唯一的屏障，readiness = 首次 Health RPC 成功
（不信裸 TCP accept，D5.2 要点①）；转换锁全程持有 ①–⑦（M1 即便单线程也要把锁的位置留对，M2 填真并发）。

---

## 4. M1 验收（可执行，过了就算 walking skeleton 成立）

一个端到端测试脚本（`test/` 下），在 machine `sbx` 上真跑 runsc，断言：

1. **create**：从一个 Template 目录 create 一个沙箱 → 拿到不透明 `sandbox_id`，state=running。
2. **overlay 生效**：merged rootfs 里能看到 Template 的内容（lower 只读底座生效）。
3. **process**：`process.Start /bin/sh -c 'echo SKELETON_OK'` → 从 server-stream 收到 `SKELETON_OK`
   与 exit_code=0。
4. **fs.write**：`fs.write /home/marker.txt = <随机串 R>` 成功。
5. **pause**：pause → 容器停（runsc list 里没了 / 探活 L1 失败），upper 目录仍在后端。
6. **resume**：resume → **新** netns IP（≠ pause 前那个），state=running，`sandbox_id` **不变**。
7. **持久性 + 身份**:resume 后 `fs.read /home/marker.txt` == R（Overlay 字节跨 pause/resume 存活，
   §4.2），且仍是同一 `sandbox_id`（§3.3 身份不编码 placement）。
8. **destroy**：destroy → 容器停 + upper 目录被回收（M1 同步删即可，M2 再做 refcount 异步 sweep）。

**全绿 = M1 通过。** 任何一条红都在评审门停下，按 CLAUDE.md 的「失败两次就 root-cause，不打补丁」
系统性分析。

---

## 5. 目录结构（canonical 短名，对齐 §5）

```
engine/
├── cmd/
│   ├── orchestrator/   单二进制 engine 宿主(§1.1);持控制面 + mediator + 各子系统机制
│   └── sandboxd/       in-guest daemon:init 模式(PID1)+ transport 模式(process/fs 终点,§5.E.1)
├── internal/
│   ├── model/          沙箱身份、lifecycle-state、三态(§3);纯类型,不依赖任何子系统
│   ├── lifecycle/      §4 mediator:状态机 + 转换锁 + CAS + 冷启链编排(动词归这里)
│   ├── store/          控制面记录 seam:lifecycle-state 读 + 条件写;M1 内存,M2 SQLite
│   ├── runtime/        §5.A:runtime provider seam + local_runsc(runsc 进程起停 + placement)
│   ├── rootfs/         §5.B:FS provider seam + overlayfs 组装(lower/upper/work → merged)
│   └── transport/      §5.E:宿主侧 process/fs client + Health;wire(Connect-RPC over h2c)
├── docs/
│   ├── spec/                §1–§6 架构规格 + deep-dives
│   ├── plans/M1-sandbox-lifecycle.md   ← 本文
│   └── decisions/           ADR
└── test/               M1 端到端验收(§4 的 8 条断言)
```

`Rootfs`(§5.B 子系统) / `fs`(§5.E 的 wire 服务) / `FS provider`（§5.B/§5.C 存储物化 seam）
三者刻意区分，勿混（§1.1 术语表）。

---

## 6. 环境前提（记忆 + 复现）

- 跑在 OrbStack machine `sbx`（arm64 / 内核 7.0.11 / cgroup v2 / overlayfs / netns / runsc systrap 非特权，
  均已实测）。engine 代码 + 所有 overlay scratch 必须在**原生盘** `/home/hsw/engine`，**不能**在共享挂载
  `/Users/...`（virtiofs 上 overlayfs upper/work 不工作）。
- runsc 已装 `/usr/local/bin/runsc`（systrap，无 KVM）。
- **架构差**：`sbx` 是 arm64，`docs/plan` 里两条载体的 ECS 验证多半是 amd64。M1 在 arm64 复跑即等于
  在第二个架构上复验那两条 substrate 事实（overlayfs 持久 + netns TCP transport）——是加分，不是风险；
  但上线前仍要在 amd64 正式复验。
