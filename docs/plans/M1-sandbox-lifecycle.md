# M1 — 单沙箱生命周期(create / pause / resume / destroy 端到端)

> **这份文档是什么。** engine 的 **M1 前瞻计划**——只回答四件事:这一刀切到哪(scope)、
> 开工前要先备好什么(Definition of Ready)、按什么次序开工(risk-first 分解)、怎么算通过
> (acceptance)。架构规格在 `docs/spec/`(§1–§6),与本文冲突**不就地调和**——按 `CLAUDE.md`
> 「Relationship rule」停下分析(见 §9)。本文随里程碑推进而失效(`docs/plans/README.md`)。
> **§** = spec 全局章节(§3.3、§5.E.1…);**D** = 对应 deep-dive 小节(D5.1、D4.4…),两套编号不混。

---

## 1. 唯一目标:一条竖切打穿最难的五个关节

M1 是单机、无 Volume / Network egress / Promote / 凭证的一条最薄竖切。它的价值不在功能多,而在
**一刀同时触到引擎最难的五个关节**,在真机(ADR-0002 的 `sbx`)上把接缝钉死:

```
Template(一个普通目录)
   │  ① §5.B Rootfs:宿主内核 overlayfs 拼 lower(Template)/upper/work → merged
   ▼
④ §5.A Runtime:local_runsc 契约式拉起,产出 placement 句柄 + transport 端点
   │  runsc --overlay2=none --file-access=shared;entrypoint = sandboxd(init 模式当 PID1)
   ▼
⑤ §5.E.1 guest 启动:PID1 装 reaper/信号 → re-exec 进 transport 模式 → netns 内 TCP listen
   │
   ▼
⑥ §5.A placement:宿主发现容器当前 netns veth IP
   │
   ▼
⑦ §4 探活门:向 IP:port 发 Health RPC,轮询至首次成功 → state=running → API 返回
   │
   ▼
process.Start /bin/sh 流回 stdout  ·  fs.write 落一个 marker 文件
   │  §5.E:process / fs 两个无状态 RPC 服务 + Health 探针
   ▼
pause(停容器、upper 留后端,§4.2 零捕获)
   ▼
resume(同一 upper 冷启新容器,换新 netns IP,§3.3)
   ▼
fs.read 读回 marker → 字节一致 ✅ 竖切成立
```

**五个关节**(这一刀过了,后续都是加厚已验证的接缝,不是在未验证地基上叠功能):

| # | 关节 | 锚点 | 为什么是「最难」 |
| --- | --- | --- | --- |
| 1 | runtime 起停 + placement | §5.A | provider 契约要契约式表达、不泄漏宿主原语,唯一实现 local_runsc |
| 2 | overlay 组装 + pause/resume 零捕获 | §5.B / D3 | 外层内核 overlayfs 拼 merged,pause 什么都不做、resume 重挂同一 upper |
| 3 | transport + PID1/transport 分进程 | §5.E / §5.E.1 / D4 | 自研最小 PID1 init + re-exec 出 transport 子进程 + netns 内 TCP |
| 4 | mediator + 转换锁 + 失败 unwind | §4 / §4.4-A/B | 动词按序调子系统、转换锁全程持有冷启链、失败原子退回源态 |
| 5 | 身份不编码 placement | §3.3 | resume 换新 IP 仍是同一 `sandbox_id` |

竖切链是 D5.1 canonical 冷启链(①–⑦)的 M1 裁剪版:**保留 ①④⑤⑥⑦,去掉 ②Volume / ③Network**
(见 §5)。

---

## 2. 两种手法分层:先买断集成风险,再加厚接缝

M1 用两种手法,作用在两个不同层,别混:

- **Spike(一次性买断)**——开工第一步(任务 A)是一条**手搓、非长存**的竖切,只为在真机上一次性
  retire 掉 substrate 集成风险(overlay→runsc→guest 拓扑→netns transport→Health 是否真能贯通)。
  它跑通即弃,产物是任务 B–E 的**活规格**(记下确切的 runsc argv、netns/veth 拓扑、IP 发现方式)。
  失败在这里最便宜。
- **Tracer bullet(加厚接缝)**——任务 B–F 把 spike 验证过的每一段落成**长存的、走接口的**真实现,
  串成一条最薄但完整的端到端竖切。后续里程碑(M2+)在这条已贯通的竖切上**加厚**已验证的接缝
  (换 store 后端、加 Volume、加 egress),不重开地基。

一句话:spike 证明「能通」,tracer bullet 证明「用对的接缝通」。

---

## 3. 边界:M1 显式不做什么

以下都**不在 M1**——不是遗漏,是刻意后置到 `docs/plans/README.md` 已列明的里程碑;每条标出「为什么
能安全后置」,即它不阻塞本竖切要验证的五个关节:

| 不做 | 属于 | 为什么能安全后置 |
| --- | --- | --- |
| store 持久化 + §4.4 三不变量的**深度**(outbox / 崩溃对账 / 真并发压测) | M2 | M1 用进程内 `store.Memory` 跑通竖切,并把 CAS + 转换锁的**形状**立对(§4.4-B);真事务、事件 outbox、多线程并发压测、崩溃重启对账是独立里程碑,要配故障注入 |
| reaper + 健康态四分(两层探活 × 连接活动) | M3 | M1 手动驱动四个动词,不自动回收;探活只用 L2 的一次 Health(§4.3 的 readiness 门,非四分器) |
| Volume(挂载表 / 租约 / 直连写 regime) | M4 | 竖切不挂任何 Volume;冷启链跳过 ②(§6.1 `volumes?` 参数存在但 M1 恒空) |
| Network **egress 数据面**(gateway / deny-set / nft 引流 / 凭证注入) | M5 | 冷启链跳过 ③;M1 只建**最简 netns**(见下),不出公网、无策略面。`SteeringSpec` 恒空 |
| Promote + 分层 Template + refcount GC | M6 | Template 是一个静态目录,不 promote、不 fork;destroy 同步删 upper 即可 |
| 凭证 store seam | M7 | 无出网即无凭证注入 |
| 完整 D4 wire(`StreamInput` client-stream / multipart 旁路 / 环形回放 / keepalive) | 随后续增量 | M1 只需 `Start`(server-stream)/ `SendInput` / `Read` / `Write` / `Health` 五个能跑 |
| content-addressed diff 层 / project quota 容量帽 / pause flush 精确一致点 | M6 / 后续 | 持久单元 M1 用「留驻 upper 目录」(D3);diff 层、容量帽(D6)、flush 一致点是同一接缝上的演进,不阻塞竖切 |

**一处必须点破的 scoping 判断——③ 出网数据面在 M1 外,但 §5.E transport 需要 netns veth 可达。**
载体是「netns 内 TCP,宿主经 veth IP 直连」(§5.E / D4.1),因此 M1 **仍要建一个最简 netns + veth**
让宿主够得到 sandboxd 的监听端口——但**只到「可达」为止**:无 egress gateway、无 deny-set、无 nft 引流、
无 §5.D 策略面。这落在 `local_runsc` provider 的 `Launch` 内(placement 的一部分),`SteeringSpec` 恒空。
「最简 netns 可达」与「egress 强制」是两件事,M1 只做前者,后者整体是 M5 的安全关键工作。

> **`store.Memory` 是脚手架,不是设计**:M2 落 SQLite/bbolt + §4.4-C 原子提交契约时按同一 `store.Store`
> 接口替换,CAS 形状 M1 已立对(见 §4),上层不改。

---

## 4. 接缝:M1 就要把形状立对

三个 seam 是承重墙,M1 即便实现最小,也要把**形状**立对,否则后续里程碑无处落。它们已在 scaffold
里以接口存在,M1 的活是给出真实实现、不改接口签名(换实现 = §1.3 register-an-implementation):

- **`runtime.Provider`(§5.A 核心 seam)**——契约左边 `LaunchSpec{Rootfs, Steering, Limits, GuestUser}`、
  右边 `Placement{TransportEndpoint, handle}`。铁律:接口里**不出现** cgroup 路径 / netns fd / upper
  绝对路径这类只有本地进程给得出的宿主原语(§5.A ❌ 清单);`Placement.handle` 是 provider 私有、调用方
  只按 `SandboxID` 寻址。M1 唯一实现 `local_runsc`:`MergedPath` 这个宿主路径只在它内部消费。
- **`rootfs.Provider`(§5.B seam)**——`CreateOverlay` / `Mount` / `Unmount` / `DestroyOverlay`。对上层只暴露
  `Overlay{ID, Template}`,**不暴露** `upperPath` / `workPath`(§3.1.1 对内也 ID 寻址,宿主路径锁死在子系统内)。
  M1 唯一实现「留驻 upper 目录」(D3);content-addressed diff 层是 M6+ 演进项,靠同一接口容纳。
- **`store.Store`(Behavior seam)**——M1 的 `CompareAndSetState(id, expected, next)` 就是 §4.4-B 转换锁要用的
  条件写机制;`store.Memory` 用一把 mutex 站位 M2 的真事务。CAS 形状现在就对,M2 换后端不动接口。

**一条铁律贯穿全程(§4 开头 / engine `CLAUDE.md`)**:**动词归 §4 mediator,机制归 §5 子系统,子系统之间
永不互相调用**。M1 的 create/resume 就是 `lifecycle.Mediator` 按 D5.1 的 ①→④→⑤→⑥→⑦ 依次调
`rootfs` / `runtime` / `transport`;任何「rootfs 去戳 runtime」都是成环起点,评审直接打回。

---

## 5. 冷启链:D5.1 的 M1 裁剪版

canonical 链(D5.1)是 ①–⑦,`create` 与 `resume` 共享,只差前缀与失败出口。M1 因无 Volume(②)、
无 Network egress(③),裁剪成:

```
① §5.B Rootfs   ：create→建空 upper/work + 拼 merged;resume→重挂同一 Overlay 工件
   〔② Volume    ：M1 跳过——无挂载〕
   〔③ Network   ：M1 跳过——无 egress 数据面;④ 内只建最简 netns+veth 供 transport 可达〕
④ §5.A Runtime  ：建 placement(最简 netns/veth/IP;资源上限 M1 留空)→ 以 sandboxd(init 模式)
                   为 entrypoint 拉起 runsc(--overlay2=none --file-access=shared)
⑤ guest 启动    ：PID1 装 reaper+信号 → re-exec 出 transport 模式子进程
                   → PR_SET_CHILD_SUBREAPER → bind netns TCP → listening
⑥ §5.A placement：发现容器当前 netns veth IP
⑦ §4 探活门     ：向 IP:port 发 Health RPC,轮询至首次成功 → state=running → API 返回
```

**顺序 / 并发 / 失败约束(M1 也守)**:

- **① 必在 ④ 之前**——挂载在 runsc 启动时从 OCI spec 取、运行中不可热改(§2.5)。
- **⑦ 是唯一屏障**——「已拉起」与「running」之间只此一道门,readiness = **首次 Health RPC 成功**
  (不信裸 TCP accept:accept 可能早于 handler 接好,D5.2 要点①/D4.5)。M1 只用这一次 L2 探活作
  readiness 门,不引入 §4.3 的两层探活 + 四分器(那是 M3)。
- **转换锁全程持有 ①–⑦**——从 CAS 前置校验一直持到 ⑦ 翻 `running`(或超时 unwind),不在 guest
  listening 前提前放手(§4.4-B / D5.1)。M1 即便基本单线程,也要把锁的持有区间放对,M2 填真并发 + outbox。
- **失败出口(§4.4-A / D5.2 要点④)**:拉起 / 探活失败先回收半建 placement(netns/veth/overlay mount),
  持久态(Overlay 工件)完好。出口按动词分:**resume** 退回 `paused`(可重试);**create** 无可退静止态,
  清理半建资源(含空 Overlay)后删记录 → `destroyed`。都不引入 `failed` 中间态、不做补偿事务。
  **这条 M1 就要真做(不是留 TODO)**——它是关节 4 的一半,且 §8 有专门断言(A9)封它。
- **事件发布不在 M1**——§4.4-C 的 transactional outbox + event stream 是 M2;M1 只做 CAS 落态,不发事件。

---

## 6. Definition of Ready:开工前先备好(preflight)

这些是踩起来最贵、又最容易漏的前置条件。开工任务 A 之前先各跑一遍确认——**它们不通,后面每一步都在流沙上**:

| # | 前置 | 为什么必须先钉 | 判定 |
| --- | --- | --- | --- |
| R1 | **overlay 全链落在 native fs,不在 virtiofs 共享盘** | overlayfs upper/work 在 virtiofs 上不工作(`CLAUDE.md` 执行约束);引擎代码 + 全部 overlay scratch 都要在 `sbx` 的 native fs | 在选定 scratch 目录上真跑一次 overlayfs mount + 写入,成功 |
| R2 | **runsc 就绪 + flag 契约可兑现** | §5.A 钉死整套 flag、不吃默认;默认跨版本漂移,§5.B 可移性不能建在「默认不变」上 | `runsc --version` 记录版本;`--platform=systrap` `--network=sandbox` `--overlay2=none --file-access=shared` `--systemd-cgroup=false` 能被本机 runsc 接受 |
| R3 | **可重复的清理脚本(teardown)** | 竖切反复跑会留孤儿 netns/veth/mount/cgroup/runsc 容器;不能清就没法重复验证,也预演了 §4.4-A 的对账语义 | 一条脚本枚举并拆掉本机由本引擎建的 netns/veth/overlay-mount/runsc 容器,幂等 |
| R4 | **arm64/amd64 复验说明就位** | ADR-0002 要求跨架构复验;M1 是 substrate 竖切,架构相关坑要早暴露 | 按 ADR-0002 记录复验计划,不在此复述 |

R1–R3 各写成 `test/` 下一个可重跑的 preflight 检查,任务 F 的 e2e 复用它们做 setup/teardown。

---

## 7. Risk-first 任务分解

原则:**最不确定的集成先打**。M1 的风险几乎全在 substrate 集成侧(overlay→runsc→guest 拓扑→netns
transport→Health),而 mediator / store 是低集成风险的编排逻辑。故先用一条**手工 spike(§2)**一次性
retire 掉 substrate 集成风险,再自底向上把各 seam 落成真实现,最后由 mediator 串起、e2e 封口。

| 序 | 任务 | 落点(scaffold) | de-risk 的关节 | done-when |
| --- | --- | --- | --- | --- |
| **A** | **手工竖切 spike**(§2 一次性买断):手搓 overlay merged 目录 + 最简 netns/veth,以 sandboxd 为 entrypoint 拉起 runsc,transport 模式在 netns TCP listen,宿主 dial veth IP 打通一次 Health RPC | 一次性脚本(`test/` 下,跑通即弃,非长存代码) | 3(guest 拓扑 + netns transport)+ 1、2 的集成 | 宿主对 guest 的 Health RPC 首次成功——substrate 全链贯通,且**确切 runsc argv / netns 拓扑 / IP 发现方式记进 B–E 的活规格** |
| **S** | **wire schema 先行**(schema-first):定 Connect-RPC IDL——`process`(`Start`/`SendInput`/`SendSignal`/`List`)、`fs`(`Read`/`Write`)、`Health`;生成 host client stub + guest server stub | `internal/transport`(生成码)+ IDL 源 | 3 的契约面(D4) | IDL 编译过,两端 stub 生成;host `transport.Client` 接口与生成码对齐 |
| **B** | **`rootfs` overlayfs provider**:`CreateOverlay` / `Mount`(拼 lower/upper/work→merged)/ `Unmount` / `DestroyOverlay`,宿主路径锁死内部 | `internal/rootfs` | 2(§5.B) | merged 里见 Template 内容;停后 upper 是宿主普通目录树、可重挂 |
| **C** | **`local_runsc` runtime provider**:`Launch`(建最简 netns/veth + 拉 runsc + ⑥ 发现 IP)/ `Stop` / `Destroy`,契约式不泄漏宿主原语;flag 按 R2 契约钉死 | `internal/runtime` | 1、5(§5.A / §3.3 IP 重绑) | Launch 返回带 `TransportEndpoint` 的 placement;Stop 释放 RAM/CPU、upper 不动;Destroy 拆净 placement-local 资源 |
| **D** | **transport 两端实现**(填 S 的 stub):`sandboxd` init 模式(PID1 reaper/信号/re-exec/监督)+ transport 模式(netns TCP server 接 `process`/`fs`/`Health`);宿主侧 `transport.Client` | `cmd/sandboxd` + `internal/transport` | 3(§5.E / §5.E.1 / D4) | `Start /bin/sh` 流回 stdout;`Write`+`Read` 往返一致;PID1 能原地重启 transport 子进程 |
| **E** | **`lifecycle` mediator**:`Create`/`Pause`/`Resume`/`Destroy` 按 ①④⑤⑥⑦ 编排,转换锁全程持有 + CAS 前置校验 + **失败 unwind 回源态**(§5 失败出口) | `internal/lifecycle` | 4(§4 / §4.4-A/B) | 四个动词端到端可驱动;并发 destroy 在 resume 持锁期拿不到锁;拉起失败 resume 退 `paused`、create 删记录 |
| **F** | **orchestrator 装配 + e2e 验收**:`cmd/orchestrator` 装配 `store.Memory`+两个 provider+mediator、暴露最简 §6.1 API;`test/` 写 §8 断言脚本,在 `sbx` 真跑 runsc,复用 R1–R3 preflight | `cmd/orchestrator` + `test/` | DoD 封口 | §8 断言全绿 |

次序说明:A 一次性买断 substrate 集成风险,其产物是活规格;S 定死 wire 契约,让 D 的两端从同一 IDL
生成、同步开发;B→C→D 自底向上落实现;E 串成动词并真做失败 unwind;F 封口。**A 与 S 无依赖,可并行起步**;
D 依赖 S(拿 stub)与 A(拿拓扑)。

---

## 8. 测试金字塔:四层,不只一条 e2e

`CLAUDE.md`「correctness lives in concurrency/failure invariants, not at compile time」——绿的 build 什么
都不证明。M1 按四层验证,越靠下越多、越靠上越贵:

- **L1 seam 单测(纯逻辑,不碰 substrate)**:`store.Memory` 的 CAS 语义(expected≠current → `ErrConflict`)、
  mediator 的转换锁互斥 + CAS 前置校验、失败 unwind 的清理次序(用 fake provider 注入失败,断言半建资源被拆、
  态回源)。这层跑得快、不依赖 `sbx`,是关节 4 的主要证据。
- **L2 子系统集成(在 `sbx`,单子系统)**:rootfs provider 真拼 overlayfs(断言 CoW + 重挂字节一致);
  local_runsc 真拉 runsc(断言 Launch/Stop/Destroy + IP 发现);transport 两端真跑一次 `Start`/`Write`/`Read`。
- **L3 e2e 竖切(在 `sbx`,全链)**:§8.1 的 happy-path 八条断言。
- **L4 故障注入(在 `sbx`,风险接缝)**:§8.2——冷启失败 unwind。这是 M1 唯一强制的故障注入,
  因为冷启链是 M1 最危险的接缝。

---

## 9. 验收(可执行的 DoD)

在 ADR-0002 的 `sbx` 上真跑 runsc,复用 §6 的 R1–R3 preflight 做 setup/teardown。**全绿 = walking
skeleton 成立**。每条标出它证明五关节里的哪一个。

### 9.1 happy-path 竖切(L3)

| # | 断言 | 证明关节 |
| --- | --- | --- |
| A1 | **create**:从一个 Template 目录 create → 拿到不透明 `sandbox_id`,state=`running` | 1 全链 + 4 |
| A2 | **overlay 生效**:merged rootfs 里能读到 Template 的内容(lower 只读底座生效) | 2(§5.B) |
| A3 | **process**:`process.Start /bin/sh -c 'echo SKELETON_OK'` → server-stream 收到 `SKELETON_OK` + exit_code=0 | 3(§5.E) |
| A4 | **fs.write**:`fs.write /home/marker.txt = <随机串 R>` 成功 | 3(§5.E) |
| A5 | **pause**:pause → 容器停(runsc list 里没了)、upper 目录仍在后端 | 2(§4.2 零捕获) |
| A6 | **resume 换 IP**:resume → **新** netns veth IP(≠ pause 前),state=`running`,`sandbox_id` **不变** | 5(§3.3) |
| A7 | **持久性 + 身份**:resume 后 `fs.read /home/marker.txt` == R,且仍是同一 `sandbox_id` | 2 + 5(§4.2 / §3.3) |
| A8 | **destroy**:destroy → 容器停 + upper 目录被回收(M1 同步删) | 1 + 2(§4.1) |

### 9.2 失败与并发 shape(L1 + L4)

happy path 只证明「顺的时候通」;关节 4 的一半是「不顺的时候不烂」。这两条 M1 也要封:

| # | 断言 | 层 | 证明关节 |
| --- | --- | --- | --- |
| A9 | **冷启失败原子退回**:注入 ④/⑦ 失败(runsc 拉不起 / Health 永不通并超时)→ 半建 placement(netns/veth/overlay mount)被回收、持久态(upper)完好;**resume** 场景态回到 `paused`,**create** 场景删记录 → `destroyed`;无 `failed` 中间态 | L4(`sbx`)+ L1(fake provider) | 4(§4.4-A / D5.2 要点④) |
| A10 | **CAS + 锁 shape**:对 `paused` 沙箱并发发 `resume` 与 `destroy`——先拿锁者推进,另一者阻塞等锁、对已变的态重做 CAS 被拒(`ErrConflict`);且 resume 持锁期间 destroy 拿不到锁,不会删正被冷启的 upper | L1(mediator 单测) | 4(§4.4-B) |

> A10 只验**形状**(锁互斥 + CAS 前置校验对);真并发压测、outbox、崩溃重启对账是 M2 配故障注入做,不越界(§3)。

任何一条红都在评审门停下,按 `CLAUDE.md`「失败两次就 root-cause、不打补丁」做系统性分析。
验证前置(`go build` / `go vet` + `sbx` 上真跑)与 arm64/amd64 复验说明见 ADR-0002 与 §6,不在此复述。

---

## 10. 分歧捕获:walking skeleton 撞到 spec 时怎么办

竖切正是**最可能撞出 spec 欠定/写错**的地方(冷启时序的精确一致点、IP 发现的确切机制、flag 与 runsc
版本的耦合……)。按 `CLAUDE.md`「Relationship rule」,撞到就**停下分析,不就地把 spec 改成迁就代码、
也不硬把代码拧成迁就可疑的 spec**:

1. **停**——判断是 spec 表达的意图对、代码错,还是实现教会我们 spec 意图本身欠定/错。
2. **决定哪边动**——修代码达意 OR 作为**独立、有据**的决定改 spec(真设计变动 = 一次 deep-dive /
   open-decisions / ADR,不是塞进 feature commit 的一行)。
3. **落**——已定的回写 spec/deep-dive、未定的登记 `open-decisions.md`,两边不重叠。

M1 预计会喂给 `open-decisions.md` 的候选(撞到再登记,不预先展开):pause flush 精确一致点(D5)、
IP 发现的确切机制(§5.A)、runsc 版本 pin 与 flag 契约的落地细节(R2)。
