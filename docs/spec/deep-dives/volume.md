# Volume:后端 seam / 写入 / 快照 / 租约的推导与证伪

> **定位:[`§5.C`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))与 [`§3.4`](../model.md)、
> [`§4.2`](../model.md)、[`§4.3`](../model.md) 的 Volume 深挖。** §5.C 载 Volume 的模型与已定契约;本文
> 留深挖该留的三件事:每条契约**为什么是这个形状**、**排除了哪些备选(证伪)**、**契约边界外待办**
> (登记 [`open-decisions.md`](../open-decisions.md))。
>
> **方法论(engine CLAUDE.md):** 从"设计该是什么样"正向推导。参考实现 sandbox0 是**分布式 / K8s /
> 对象存储**系统:它的 WAL→对象存储物化、内容寻址层、跨 node 心跳回收、head CAS、应用层加密,都是
> **"POSIX over 一个远端、非 CoW、不可信对象存储"**的产物——解决单机引擎**根本没有的问题**。逐条从
> 第一性原理重新推导:凡属**远端后端实现义务**的,封进"那个 backend"、不进核心;核心只认"Volume 经
> backend seam 物化成一个宿主挂载点"。
>
> **两条已在真实 substrate 验证的地基**(preflight spike,`test/preflight/`):① runsc
> `--overlay2=none --file-access=shared` 会把一个**额外的 rw 宿主目录 bind mount** 经 gofer 双向代理进
> guest(local 后端地基,`spike-volume-mount.sh`);② 一个**宿主侧 FUSE 挂载点**同样能经 gofer 代理进
> guest(remote 后端地基,`spike-fuse-passthrough.sh`)。下文的 seam 形状建立在这两条事实上。

---

## D0. 结论:一条 backend seam,两条物化臂

Volume 相对 §5.B 的 Overlay 只多几个属性,每个属性对应一条契约(正文在 §5.C)和一处证伪。**统摄全篇
的形状**:Volume 经一个可插拔 **backend seam** 物化,seam 只吐**一样东西——一个宿主挂载点**;runsc 把
它写进 OCI mount spec、gofer 代理进 guest,与 §5.B 的 merged rootfs 完全同构。backend 有两条臂:

| 臂 | 物化 | 谁兑现 POSIX | 验证 |
| --- | --- | --- | --- |
| **local(默认)** | 宿主上一个目录(btrfs subvolume) | 宿主内核(gofer 代理普通目录) | Spike 1 |
| **remote**(S3 / NFS / …) | 宿主侧 **FUSE** 挂载点,backend 维护、译成对后端的操作 | backend 自己在 FUSE 里兑现 | Spike 2 |

两臂**在 guest 侧零差异**(都是一条 gofer 代理的 OCI mount);差异只在"挂载点背后是普通目录还是 FUSE
挂点"。Volume 相对 Overlay 的新属性与其契约 / 证伪:

| 新属性 | 契约(详见 §5.C) | 排除的备选 |
| --- | --- | --- |
| **多后端**(local / 远端对象存储 / 网络 FS) | backend seam 吐宿主挂载点(D1/D2) | FUSE-for-everything(D1)· 把 s0fs 引擎搬进核心(D6) |
| **多写入路径**(挂载 + 直连 API) | 写入 regime 唯一(D5) | 反向代理直连写(D5) |
| **可被多沙箱并发挂** | 跨沙箱 rw coherence undefined(D4) | —(本质如此) |
| **独立快照 / 克隆** | 默认 crash-consistent、per-backend capability(D6) | 二段式 checkpoint(D6)· 用户态 segment 引擎(D6) |
| **独占** | 租约 = §4.3 挂载行(D3/D4) | 心跳双回收(D4) |
| **容量帽** | btrfs subvolume + qgroup,跨 §5.B/§5.C 共享原语(D7) | project quota(D7,substrate 不支持) |

---

## D1. 物化:统一"宿主挂载点"抽象,local=目录 / remote=host-FUSE

单机 local 后端就是宿主上一个目录,进 guest 的路径与 §5.B 的 merged rootfs **完全同构**——写进 runsc
OCI mount spec、由 gofer 在 `--overlay2=none --file-access=shared` 下代理。exclusive / shared-ro /
shared-rw 都只是"同一宿主目录进一到 N 个 guest",无特殊机制(Spike 1 已证 rw 双向、目录枚举、嵌套
子目录均正确)。

### D1.1 FUSE 的正确位置:宿主侧,不进 guest;是 remote 后端的物化,不是核心机制

FUSE 不进核心、不上单机 local 路径;它是 **remote 后端**的物化手段。远端后端(对象存储 / 网络 FS)天然
需要一层"把远端译成可挂 POSIX FS",这层就是 FUSE——但它有一个不可动摇的位置约束:

> **FUSE 挂在宿主侧(backend 进程里),经 gofer 代理进 guest;绝不在 guest 内跑 FUSE。** gVisor 对
> guest 内 FUSE 的支持有限/演进中;而"宿主 FUSE 挂点 → gofer 代理"这条路已在 Spike 2 验证:gofer 会
> descend 进 FUSE 挂点、把 FUSE-backed 文件与嵌套目录当普通 POSIX 代理进 guest。sandbox0 的 ctld 也正是
> 在**宿主**挂 FUSE portal(`pkg/fuseportal` 直接对 `/dev/fuse` 说协议),不在沙箱内——同构印证。

所以 FUSE 不是"要不要进引擎"的问题,而是"**封在哪个 backend 实现里**"的问题:它是 remote backend 的
内部物化手段,与 local backend(普通目录)在 seam 之上一字不差(§3.1.1「对内也 ID 寻址」:核心只认
"Volume 经 backend 物化成一个宿主挂载点",不认背后是目录还是 FUSE 挂点)。

### D1.2 证伪:为什么不"FUSE-for-everything"(连 local 也走 FUSE)

一个诱人的统一是:**所有** Volume(含 local)都走同一个宿主 FUSE 前端 + 可插拔 `Backend{Read/Write/
List/Stat…}`,好处是缓存 / 加密 / 配额 / 审计能在 FUSE 层统一做。**不采用**,理由是成本判据:

- **local 路径的 POSIX 是白拿的**:gofer 代理一个普通宿主目录,rename / 目录 / 权限 / 锁 / mmap 全部由
  **宿主内核**兑现(Spike 1)。走 FUSE 则要在用户态**自己重新兑现 POSIX 语义**——sandbox0 的
  `volumefuse.Session` 是一个 **40 方法**的接口(Lookup/Rename/Flock/CopyFileRange/xattr/…),其 s3 后端
  对 Rename/锁/Fallocate/xattr 只能返回 `EOPNOTSUPP`,直接暴露了"自己实现 POSIX"的工程量与语义损失。
- **统一缓存 / 加密 / 配额 / 审计是分布式诉求**:加密只在数据离开信任边界(进 S3 / 共享缓存)时才必要;
  统一缓存是"对象存储无 POSIX 缓存"的补偿。单机可信节点上,配额有 btrfs qgroup(D7)、审计有 §4.1 事件流,
  都不必挤进 FUSE 层。
- **结论**:一个完整的 FUSE `Session` 接口**只为"合成型 backend"(把非 FS 的后端合成成 FS)保留**;
  local backend(真实宿主 FS)根本不需要它。所以引擎的 seam 可以很窄(D2)——统一发生在 seam(都吐宿主
  挂载点),而不是在"所有东西都走 FUSE"。

---

## D2. backend seam 形状:窄接口,吐宿主挂载点

Volume 是独立子系统、有自己的 seam `volume.Backend`,**与 §5.B 的 `rootfs.Provider` 是两个不同形状、
各自注册的接口**:§5.B 物化**分层合成**(沿 Template lineage 解析 multi-lowerdir 下层栈 + 可写 upper/work,
经内核 overlayfs 合成 merged,并支持 Promote 冻结);§5.C 物化**一个自足的独立存储单元**(直接 bind、无
合成、无下层栈、无 lineage)。二者只共享底层 substrate 原语(btrfs subvolume + qgroup + reflink/snapshot,
见 D7),不共用一个物化接口。

seam 沿用 §5.A `runtime.Provider` / §5.B `rootfs.Provider` 的既定约定:**ID 过 seam,宿主路径留 seam 内;
策略+句柄,不泄漏宿主原语**。核心动作:

- `Materialize(volumeID, accessMode) → Mounted` —— 备好一个可挂的宿主挂载点(local:确保 subvolume 存在
  并可选打 qgroup 帽;remote:拉起 FUSE 挂点)。返回一个**不透明 `Mounted` 句柄**,只携带一个 backend 私有
  的宿主路径(`SourcePath`,仅 `local_runsc` 消费,如同 `Merged.MergedPath`)。
- `Unmount(m)` —— 拆宿主侧挂载(local:no-op 或 detach;remote:umount FUSE)。**卸载不删数据**。
- `AdoptMounted(volumeID) → Mounted` —— 崩溃后重认一个仍挂着的挂载点(§4.4-A,`AdoptMerged` 的孪生)。
- `DestroyVolume(volumeID)` —— 回收该 Volume 的后端字节(terminal)。
- `SweepOrphanVolumes(live, building) → int` —— 孤儿卷字节 sweep,与 `SweepOrphanLayers` 一一同构:
  mediator 读**权威 store** 算出 live 集喂进来,backend 只枚举自己的卷、删差集(§4 子系统不读 store)。
- `Capabilities() → {Snapshot, Clone, DirectIO…}` —— **per-backend 能力查询**(D6):local(btrfs)报支持
  snapshot/clone;纯目录或某些 remote 后端可不支持,调用即 400,而非静默。

挂载进 guest 的机制:mediator 在 `coldStart` 里把每个 `Mounted` 句柄组装成 `LaunchSpec.Mounts
[]MountSource{SourcePath, Dest, ReadOnly}`(与 `RootfsSource` 平级的契约成员),`local_runsc` 在
`buildOCISpec` 里把每个 `MountSource` 追加成一条 `ociMount{Type:"bind", Source, Destination, Options}`
——正是 Spike 1 证过的形状。Volume **身份**在 seam 之上恒为 ID;宿主目录在 backend 内解析,只以不透明
`SourcePath` 浮出(§6.2 对外只认 Volume ID,不暴露宿主路径)。

---

## D3. 挂载表 = 一等 store 对象,复用 M5 对象模型(不重造)

挂载关系是 Binding(§3.4):"挂了哪些、挂到哪、什么策略"记在控制面挂载表,resume 时按表重挂。挂载表的
每一行是一个**一等持久对象**(§4.3 挂载行),它就是 **`sandboxes.template` 引用的翻倍**:

- **结构 = 双向 FK RESTRICT**:一条挂载行 `Mount{Sandbox, Volume, Path, Access}`,`sandbox` 与 `volume`
  两列各 `REFERENCES … ON DELETE RESTRICT`。这一条就白拿了"卷有活挂载行则不可删"(`ErrInUse`)与"沙箱有活
  挂载行则删不动"两个不变量——SQLite 靠 FK、Memory 靠 Go 扫描,同一套断言(§4.3 seam,复用 M5 的
  `isFKViolation`→`ErrInUse` 翻译点)。
- **存在即状态、derived refcount、无 outbox 事件**:挂载行无 state 列、无 count 列;"卷是否被独占"由挂载行
  的存在+access 派生(不物化计数)。挂载行不是 §4.1 生命周期转换,故 create/delete 挂载行**不发事件**(与
  Template CRUD 同,区别于沙箱转换)。
- **bytes-first、转换锁内**:挂载行在**每沙箱转换锁**内、随 create/resume 冷启链写入;失败被 `unwind` 回滚。

**mediator 里的挂钩点**(动词归 §4,机制归 §5 backend;子系统间不互调):

| 钩子 | 动作 |
| --- | --- |
| `Create` | 记录插入后、`coldStart` 前写挂载行(纳入 `unwind` 回滚);`CreateSpec` 增 volume-binding 入参(今天 model 里刻意省略、未 stub) |
| `coldStart`(create+resume 共用) | `Launch` 前:`Backend.Materialize` 每个卷 → 把句柄组进 `LaunchSpec.Mounts`;`liveState` 增 `mounts` 字段供 pause/destroy 释放 |
| `resumeLocked` | 从 store **读回**挂载表(挂载行跨 pause 持久)、重新 `Materialize`——与 `resolveLower` 每次重解析血缘同理,卷是同一真身、拿新的宿主物化 |
| `pauseLocked` | CAS 后:`Backend.Unmount` 每个卷(**挂载行保留**,租约不释放),对称于 Overlay「零捕获」 |
| `destroyLocked` | 卸载活卷 → **先删挂载行、再 `DeleteSandbox`**(否则 `mounts.sandbox` FK 挡住沙箱删除);这是租约释放点 |
| 启动/周期对账 | `SweepOrphanVolumes` 镜像 `SweepOrphanLayers`;`SeedBuiltinVolumes`(若有平台预置卷)镜像 `SeedBuiltinTemplates` |

**唯一比 M5 多出的新不变量:独占挂载唯一性**。M5 的 FK RESTRICT 给不了"一个卷至多一条 rw 挂载行"——这需要
在 exclusive 卷上加一条 `UNIQUE(volume) WHERE access='exclusive'` 偏索引(SQLite)+ Memory 扫描孪生,用
与 `testReferenceReclaimRace` 同样的单写者序列化闭合其 TOCTOU。这是 Volume 里程碑相对 M5 唯一的新 store 断言。

### D3.1 证伪:store-级独占 token vs 进程内 open-handle refcount,不可混

sandbox0 保留**两层** refcount:① DB 挂载行(谁独占、跨进程)② `boundVolume.refCount`(本 node 上多少个
挂载共享**同一个已打开的 backend 会话**)。第②层只在"一个 FUSE 会话服务 N 个沙箱"时才有意义——它数的是
**打开的合成 FS 句柄**的引用数。本引擎 local 后端是"gofer 各自代理一个宿主目录",**没有共享的 open
handle**,故第②层退化掉,只剩第①层(挂载行)。第②层因此是 **remote/合成 backend 的内部实现细节**,不进核心
挂载表模型——记此以免把分布式的双层 refcount 误当核心需求。

---

## D4. 访问模式与独占租约

三种访问模式,按"共享 + 可写性"的可观测属性命名,**不用 `owner`**(§1.1 词汇边界:`owner` 归属义禁作
实体名):`AccessMode ∈ {Exclusive, SharedReadOnly, SharedReadWrite}`(= sandbox0 的 RWO/ROX/RWX)。
- **Exclusive**:一卷至多一条 rw 挂载行(D3 的唯一性偏索引强制)。建 exclusive 挂载行前控制面 CAS 校验无冲突
  挂载行,有则拒。
- **SharedReadOnly**:多沙箱只读扇出。
- **SharedReadWrite**:只保证**同沙箱内**多连接(process)的 POSIX 语义;**跨沙箱 rw coherence 引擎不定义
  (undefined)**——gofer/sentry 各自缓存让跨隔离边界的共享可变一致性本质不可靠,引擎不承诺、用户文档诚实
  标出(这与 D1.2 拒 FUSE-统一缓存同源:跨边界共享可变状态是难题,回应是**不承诺**,而非造脆弱机制假装承诺)。

**独占租约 = 那条 Exclusive 挂载行的存在本身,不是独立带心跳的对象**——不引入 `Lease` 类型、不加 `owner`
字段(租约是"卷被独占挂载"这个由挂载行派生的状态,如同 Template 的 refcount 是派生的)。持有者是**挂载行
指名的那个沙箱**,释放发生在挂载行被拆时。

**崩溃保持租约是正确行为,不是漏洞**:沙箱崩溃 = dead-recoverable(§4.3,L1 探不到、持久态完好),它会
resume 回同一卷;崩溃即释放会让 resume 撞车——与 §4.2「崩溃走 resume 恢复」一字一致。租约**只在挂载行被拆
时释放**(destroy / reaper 对 paused 超 TTL 的 evict),**reaper TTL 就是"无人释放"的兜底**。

**证伪:心跳失效 + Pod 销毁双回收。** 它是**分布式无单一权威 store、node 会凭空消失**的产物(node 死了没人
知道,只能靠心跳超时推断),sandbox0 的 `registerOwner` 心跳 + `GetActiveMounts(heartbeatTimeout)` 正是此。
本引擎单机单 orchestrator、store 即权威(§1.1):没有"凭空消失的 node",崩溃是 L1 探活立即可判的本地事实。
故**留 reaper TTL 兜底、去掉周期心跳循环**(没有可交接的另一 node)。心跳式回收是未来 multi-node provider
的义务(node 死亡由其调度层接),不进核心。**唯一真残留**:长期 paused / 卡住的沙箱占着独占卷、他方现在
就想要——引擎默认独占直到 destroy/evict,不主动抢占;要不要强制 detach / 抢占是策略(大概率产品层),
登记 open-decisions(D8)。

---

## D5. 写入 regime:一条不变量,不要反向代理

hazard 是真的:卷可能同时被运行中沙箱经 gofer rw 写、被直连 API 从宿主侧写同一后端 → 撕裂,且 gofer/sentry
页缓存与直连写不 coherent。§5.C 用一条**不变量**堵掉它:

> **只要有 live-rw 挂载正持有该卷(某沙箱 `running` 且 rw),直连 API 的变更操作(import / restore-into)
> 一律拒绝**;无 live-rw 挂载时直连写才放行。门开在"**活的**写入方",不是"在挂载表里":**paused 沙箱不算
> live writer**(容器已停、无活 gofer),此时直连写安全,且 resume 后自然见到新内容。这是控制面读 store 后
> 对直连 API 的准入裁决。

**未挂载时的直连操作**(create/delete/list/inspect/import/export):local 后端直接对宿主目录/subvolume 操作,
单机场景简单,无需"惰性 attach"。sandbox0 的"lazy owner-attach + idle reclaim"(无挂载时临时开一个 backend
会话服务直连、闲置回收)是**合成型 backend 才需要的**(要一个打开的 FS 会话去服务),local 目录用不上;它作为
一个 [REMOTE-BACKEND] 模式登记即可。

**证伪:直连反向代理到挂载沙箱**(卷挂着时把直连写反代进持有沙箱,以允许"挂着也能从外面写")。**不采用**:
①这是**产品便利**、不是引擎正确性需求——引擎只要"别撕裂",§5.C 的**拒绝**就够,成本远低于造一条反向代理
数据面;②反向代理要引擎理解"谁在挂、如何穿进它的 gofer 写它的 FS",把**跨对象写入编排**拉进引擎(§1.3
litmus:非 register-impl / add-join-table / add-subscriber,而是改现有结构 → 该问"是不是不该进引擎")。
答案:要这个便利就叠一层引擎外 adapter(读挂载表 → 经 §5.E `fs` 写进持有沙箱),引擎不做。**sandbox0 恰恰
做了这个路由**(`volume_affinity.go` 把直连请求反代到挂载 owner)——但它是产品平台,把这层便利算进自己职责;
引擎划在边界外,这是**定位差异,不是它错**。

---

## D6. 快照 / 克隆:btrfs 原生 CoW,per-backend capability

### D6.1 单机用 btrfs,不移植 s0fs 的用户态 segment 引擎

sandbox0 的 s0fs 是"POSIX over 对象存储"的一整台机器:WAL(node-local 变更日志)→ 周期物化成不可变
manifest + 内容寻址 segment → head 指针 CAS,快照=引用同一批不可变 segment、fork=改写 segment 引用做 CoW。
**这台机器整体属 [REMOTE-BACKEND],不进核心**:它存在的唯一理由是"对象存储不是 POSIX、无 durability-on-
write、无 reflink、不可信"。单机 btrfs 上,**文件系统本身就给了 CoW**——`cp --reflink=always` 已验证共享
extent(同一物理 offset、`shared` 标志,即便 `nodatacow` 挂载下显式 reflink 仍生效),而 `btrfs subvolume
snapshot` 更是**整树、原子、瞬时**(实测新快照仅 16KiB exclusive 元数据、零数据拷贝)。

所以:**采纳语义模型**(快照=不可变时间点、fork=CoW 子卷、二者元数据级廉价)与**能力矩阵**(快照/fork 是
per-backend capability),**丢弃 segment/manifest 实现**——让 btrfs 当"materializer"。若把 Volume 建模成一个
**btrfs subvolume**,快照/克隆 = `btrfs subvolume snapshot`,与 D7 的容量帽(qgroup 打在 subvolume 上)
天然合一:同一个存储单元同时得到廉价快照与硬容量帽,这是"卷=subvolume"的强理由。

### D6.2 快照默认 crash-consistent,与 Overlay 不对称

Overlay 的 Promote 能强一致,因它**单一写入方**、可 pause 到 at-rest 读(§5.B)。Volume 打破此前提
(shared-rw 多写入方、挂载中不想停)。故 Volume 快照 = 后端对当前盘上字节的原子快照、**不静默写入方**,
一致性等价"那一刻拔电源"——正是 §4.2 已接受的"崩溃 = 非自愿 pause"底线。要 app-consistent:**独占卷**由
调用方先 pause 其持有沙箱(复用 §4.2 静止点)再快照,是现有原语的**组合**、引擎不加机制;**shared-rw 不
提供** app-consistent(协调 N 个沙箱是产品语义,§1.3 否决)。对挂载中的活卷做 subvolume snapshot 前应
`fsync`/freeze 挂载点以保 crash-consistent。

### D6.3 证伪:二段式 checkpoint

用"阻止新操作 + 排空飞行事务 + 强制刷盘"的二段式给挂载中卷拍一致快照——**不采用**,两头不落好:①对独占卷
冗余(二段式本质就是 §4.2 的 pause 静止点,已被"pause owner 再快照"覆盖,再造一份会让 §5.C 反向依赖
§4.2,违反子系统间不互调);②对 shared-rw 不够(N 个独立沙箱的 gofer 无共同协调者,强造一个就是把 swarm
编排塞进引擎,§1.3 否决)。

---

## D7. 容量配额:btrfs subvolume + qgroup,跨 §5.B/§5.C 共享原语

§5.C 的 per-volume 容量帽与 §5.B 的 per-sandbox Overlay upper 帽是**同一个物化问题的两处应用**(§3.4 容量
轴)。真实 substrate(ADR-0002:btrfs)上实测结论:

- **机制**:`qgroup limit` 打到 subvolume,写满触发**精确到帽的硬 `ENOSPC`**(连 `mkdir` 都拒);qgroups 在
  本盘已 fs-wide 启用,无需额外挂载。overlayfs upper/work 落在 btrfs subvol 上正常(含 copy-up),且因
  upper+work 同处一个 subvolume → 一个 qgroup 天然覆盖两者(满足 rootfs-overlay D6.3「work 必须与 upper 同
  配额」约束,无绕过路径)。
- **loop 设备**是"需强物理隔离"的 fallback(本盘亦可跑);btrfs 无 project quota,故 project-quota 类手段
  不适用本 substrate(详见 rootfs-overlay D6.2)。

**归属**:容量帽是一个**共享存储 substrate 原语**(§5.B overlay upper 与 §5.C per-volume 同用 create-subvolume
+ set-qgroup-limit + delete-subvolume),跨两个子系统复用。它作 Volume 的**共享内部代码**落地(Volume 是首个
消费者:它本就要把存储单元定为 subvolume 以兑现廉价快照);§5.B 复用同一 helper 给 overlay upper 打帽,是随后
一个小 hardening,不作独立里程碑一级(帽无消费者则非可验收 capability)。它够轻(两条 `btrfs` 子命令、qgroup
已启用)。**caveat**:`btrfs-progs` 在 `sbx` 默认未装,实现须显式加此依赖;且本引擎不是该盘 qgroup
命名空间的唯一所有者(OrbStack 也用),嵌套 subvol/qgroup 实测共存无碍。

---

## D8. 契约边界外待办(登记 [`open-decisions.md`](../open-decisions.md),不在本文拍板)

以下是设计已定、但**具体取值 / 落地形态未定**的边界项(产品层策略,或落 §6 时定):

- **独占卷的抢占 / 强制 detach 策略**(D4):默认独占直到 destroy/evict;要不要 admin 强制 detach、抢占
  优先级——策略,大概率产品层。
- **shared-rw 打到 per-volume 帽 / ENOSPC 的复合行为**:多沙箱共写一卷、卷写满时各 guest 见到的 errno、
  是否发容量压力事件——与 rootfs-overlay D6.5 的 per-sandbox ENOSPC 同族。
- **直连变更 vs live-ro reader 的准入细节**(D5 边界):有 live-**ro** 挂载时允许直连写会让 ro reader 的
  gofer 缓存与后端不 coherent;要不要加告警 / 版本戳 / 拒绝,精度问题。
- **snapshot/clone 一致性标注如何进 §6 契约**(D6):API 是否显式标注"本快照 crash-consistent"、clone 出的
  卷是否继承 quota/策略——落 §6 时定。
- **远端 backend 的落地形态**(D1/D2/D6):S3 / NFS backend 何时、以何形状落;其内部(host-FUSE 前端、
  完整 `Session` POSIX 接口、以及若接对象存储则 WAL/manifest/segment/内容寻址/加密/GC 等 s0fs 式机制)
  **全封在该 backend 内**,不进核心——本文只确保 seam 形状容得下它。
