# Volume:后端 seam / 写入 / 快照 / 租约的推导与证伪

> **定位:[`§5.C`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))与 [`§3.4`](../model.md)、
> [`§4.2`](../model.md)、[`§4.3`](../model.md) 的 Volume 深挖。** §5.C 载 Volume 的模型与已定契约;本文
> 留深挖该留的三件事:每条契约**为什么是这个形状**、**排除了哪些备选(证伪)**、**契约边界外待办**
> (登记 [`open-decisions.md`](../open-decisions.md))。
>
> **本文反复排除的一整族机制**(WAL→对象存储物化、内容寻址层、跨 node 心跳回收、head CAS、应用层加密)
> 都是 **"POSIX over 一个远端、非 CoW、不可信对象存储"**的产物——解决单机引擎**根本没有的问题**。判据:凡属
> **远端后端实现义务**的,封进"那个 backend"、不进核心;核心只认"Volume 经 backend seam 物化成一个
> 宿主挂载点"。
>
> **两条 substrate 事实(实测)**:① runsc `--overlay2=none --file-access=shared` 会把一个**额外的 rw
> 宿主目录 bind mount** 经 gofer 双向代理进 guest(local 后端地基);② 一个**宿主侧 FUSE 挂载点**同样能
> 经 gofer 代理进 guest(remote 后端地基)。下文的 seam 形状建立在这两条事实上。

---

## D0. 结论:一条 backend seam,两条物化臂

Volume 相对 §5.B 的 Overlay 只多几个属性,每个属性对应一条契约(正文在 §5.C)和一处证伪。**统摄全篇
的形状**:Volume 经一个可插拔 **backend seam** 物化,seam 只吐**一样东西——一个宿主挂载点**;runsc 把
它写进 OCI mount spec、gofer 代理进 guest,与 §5.B 的 merged rootfs 完全同构。backend 有两条臂:

| 臂 | 物化 | 谁兑现 POSIX | 实测 |
| --- | --- | --- | --- |
| **local(默认)** | 宿主上一个目录(btrfs subvolume) | 宿主内核(gofer 代理普通目录) | 已验 |
| **remote**(S3 / NFS / …) | 宿主侧 **FUSE** 挂载点,backend 维护、译成对后端的操作 | backend 自己在 FUSE 里兑现 | 部分已验(读/枚举路径实测;写路径与真实后端未测,见 D8) |

两臂**在 guest 侧零差异**(都是一条 gofer 代理的 OCI mount);差异只在"挂载点背后是普通目录还是 FUSE
挂点"。

**remote 臂那半未测不构成 seam 形状的未验前提,故不进 open-decisions。** seam 形状只依赖**穿透性**
——gofer 会 descend 进宿主侧 FUSE 挂点、POSIX 语义穿透——读/枚举已证足以立住这条;写路径的行为是**该
backend 自己的验收项**(D8),不是核心模型的前提。Volume 相对 Overlay 的新属性与其契约 / 证伪:

| 新属性 | 契约(详见 §5.C) | 排除的备选 |
| --- | --- | --- |
| **多后端**(local / 远端对象存储 / 网络 FS) | backend seam 吐宿主挂载点(D1/D2) | FUSE-for-everything(D1)· 把用户态 segment 引擎搬进核心(D6) |
| **多写入路径**(挂载 + 直连 API) | 写入 regime 唯一(D5) | 反向代理直连写(D5) |
| **可被多沙箱并发挂** | 跨沙箱 rw coherence undefined(D4) | —(本质如此) |
| **独立快照 / 克隆** | 默认 crash-consistent、per-backend capability;产出**普通可写卷 + `parent` lineage**,帽必须显式声明(D6/D6.1a/D7.3) | 二段式 checkpoint(D6)· 用户态 segment 引擎(D6)· 只读快照类型(D6.1a)· 继承源的帽(D6.1a) |
| **独占** | 租约 = §4.3 挂载行(D3/D4) | 心跳双回收(D4) |
| **容量帽** | btrfs subvolume + squota,跨 §5.B/§5.C 共享原语(D7) | project quota(D7,substrate 不支持) |

---

## D1. 物化:统一"宿主挂载点"抽象,local=目录 / remote=host-FUSE

单机 local 后端就是宿主上一个目录,进 guest 的路径与 §5.B 的 merged rootfs **完全同构**——写进 runsc
OCI mount spec、由 gofer 在 `--overlay2=none --file-access=shared` 下代理。exclusive / shared-ro /
shared-rw 都只是"同一宿主目录进一到 N 个 guest",**无特殊机制**。实测覆盖(单挂载与**并发**多挂载两半):

- 单挂载:rw 双向、目录枚举、嵌套子目录均正确。
- **两个沙箱并发 rw 挂同一个 source 都成功**——无 EBUSY、无"source in use",两个 gofer 都不抱怨,且第二个
  挂载**真的可写**、不是被静默降级。**这是 seam 之下最关键的那半地基**:第二个 gofer 若无法 attach,
  SharedReadOnly 的读扇出与 SharedReadWrite 就不可实现,AccessMode 枚举会塌缩成只剩 Exclusive。
- ro 臂可与两个活 rw 臂并存、guest 内 `statfs` 报只读、写入失败于 `EROFS`(干净的 POSIX 拒绝,不是 EIO、
  不是 fault),且**不静默成功**(宿主始终看不到那次写)。
  - **但这条只对"可写单元 + ro 挂载"成立;反向不是运行时错误,是启动失败。** 拿一个**只读** btrfs 快照
    配 rw 挂载规格,`runsc create` 直接失败在 gofer 挂载阶段(`creating gofer mounts: … EROFS`),容器
    没被创建、guest 从未运行。分类上这是**控制面失败**,不是数据面 `EROFS`——比静默成功好,但它是**另一种**
    失败模式:冷启会把一个"本该在准入期以 4xx 拒掉"的配置错误,表现成一个不透明的 mount 失败。
    所以 ro 的方向性得写进 §5.C,不能靠"ro 由运行时强制"一句话覆盖两向。
  - 顺带否掉一个可能的实现路径依赖:只读快照可用 `btrfs property set … ro false` 解冻,解冻后客户端写入
    成功、**帽穿过 ro→rw 转换存活**(与 D7.3 的 Q3 独立同向)。故 `Clone` **可以**实现成 snapshot-then-unfreeze
    ——基底不强迫我们一开始就产出非只读快照,snapshot 与 clone 因此能共用一条后端路径(D2)。
- **挂载互相独立**:kill + 强制 delete 掉其中一个沙箱后,另一个仍能读**和写**,ro 臂仍可读——一个沙箱的
  拆除不扰动对端 gofer 对同一 source 的视图,故 destroy 不是跨沙箱副作用。
- **跨沙箱 coherence 观测到了,但不进契约**:每次跨读都成功、ro guest 在半秒轮询内看到了 later rw 写入,
  机制上也说得通(此模式下每个 gofer 代理同一批宿主 dentry、sentry 侧无页缓存),故它**很可能**继续这样。
  但那是关于某个 runsc build 的事实,不是承诺(D4);把今天的 gofer 缓存行为钉进测试恰好会制造一条
  **假不变量**,而诚实的立场是 undefined。

### D1.1 FUSE 的正确位置:宿主侧,不进 guest;是 remote 后端的物化,不是核心机制

FUSE 不进核心、不上单机 local 路径;它是 **remote 后端**的物化手段。远端后端(对象存储 / 网络 FS)天然
需要一层"把远端译成可挂 POSIX FS",这层就是 FUSE——但它有一个不可动摇的位置约束:

> **FUSE 挂在宿主侧(backend 进程里),经 gofer 代理进 guest;绝不在 guest 内跑 FUSE。** gVisor 对
> guest 内 FUSE 的支持有限/演进中;而"宿主 FUSE 挂点 → gofer 代理"这条路已实测:gofer 会
> descend 进 FUSE 挂点、把 FUSE-backed 文件与嵌套目录当普通 POSIX 代理进 guest。(成熟的 FUSE-portal
> 实现同样挂在**宿主**侧、直接对 `/dev/fuse` 说协议,不在沙箱内。)

所以 FUSE 不是"要不要进引擎"的问题,而是"**封在哪个 backend 实现里**"的问题:它是 remote backend 的
内部物化手段,与 local backend(普通目录)在 seam 之上一字不差(§3.1.1「对内也 ID 寻址」:核心只认
"Volume 经 backend 物化成一个宿主挂载点",不认背后是目录还是 FUSE 挂点)。

### D1.2 证伪:为什么不"FUSE-for-everything"(连 local 也走 FUSE)

一个诱人的统一是:**所有** Volume(含 local)都走同一个宿主 FUSE 前端 + 可插拔 `Backend{Read/Write/
List/Stat…}`,好处是缓存 / 加密 / 配额 / 审计能在 FUSE 层统一做。**不采用**,理由是成本判据:

- **local 路径的 POSIX 是白拿的**:gofer 代理一个普通宿主目录,rename / 目录 / 权限 / 锁 / mmap 全部由
  **宿主内核**兑现。走 FUSE 则要在用户态**自己重新兑现 POSIX 语义**——一个完整的 FUSE 会话接口是
  **数十个方法**(Lookup/Rename/Flock/CopyFileRange/xattr/…),而对象存储后端对 Rename/锁/Fallocate/xattr
  只能返 `EOPNOTSUPP`:这就是"自己实现 POSIX"的工程量与语义损失。
- **统一缓存 / 加密 / 配额 / 审计是分布式诉求**:加密只在数据离开信任边界(进 S3 / 共享缓存)时才必要;
  统一缓存是"对象存储无 POSIX 缓存"的补偿。单机可信节点上,配额有 btrfs squota(D7)、审计有 §4.1 事件流,
  都不必挤进 FUSE 层。
- **结论**:一个完整的 FUSE `Session` 接口**只为"合成型 backend"(把非 FS 的后端合成成 FS)保留**;
  local backend(真实宿主 FS)根本不需要它。所以引擎的 seam 可以很窄(D2)——统一发生在 seam(都吐宿主
  挂载点),而不是在"所有东西都走 FUSE"。

---

## D2. backend seam 形状:窄接口,吐宿主挂载点

Volume 是独立子系统、有自己的 seam `volume.Backend`,**与 §5.B 的 `rootfs.Provider` 是两个不同形状、
各自注册的接口**:§5.B 物化**分层合成**(沿 Template lineage 解析 multi-lowerdir 下层栈 + 可写 upper/work,
经内核 overlayfs 合成 merged,并支持 Promote 冻结);§5.C 物化**一个自足的独立存储单元**(直接 bind、无
合成、无下层栈、无 lineage)。二者只共享底层 substrate 原语(btrfs subvolume + 配额 + reflink/snapshot,
见 D7),不共用一个物化接口。

seam 沿用 §5.A `runtime.Provider` / §5.B `rootfs.Provider` 的既定约定:**ID 过 seam,宿主路径留 seam 内;
策略 + 句柄,不泄漏宿主原语**。

> **一条贯穿本节的形状规则:「分配」「设帽」「放置」是三个动作,不能压进一个调用。** 把它们合并会让创建、
> 容量、准入项三者都无处安放——`DestroyVolume` 找不到创建的对手方、"别动配额"需要一个哨兵值、无帽卷的字节
> 无人报告。这条规则是**用第二个 backend 验出来的**:一个 test-only 的 plain-directory 臂与 btrfs 臂跑同一份
> 共享契约用例(双臂全过),而合并形状在这三处各响一次。故 seam 里始终**保留一个 test-only 的第二臂** ——
> 它不是凑数,它是这个 seam 唯一的证伪手段。

核心动作:

- `Materialize(ctx, volumeID) → Materialized` —— 备好一个可挂的宿主挂载点(local:确保存储单元存在;
  remote:拉起 FUSE 挂点)。返回一个**不透明句柄**,只携带一个 backend 私有的宿主路径(`SourcePath`,
  仅 `local_runsc` 消费,如同 `Merged.MergedPath`),外加一个**非导出的 backend 私有 handle**
  (`SetHandle`/`Handle`,`Placement.handle` 的先例)——FUSE 臂要起/停一个进程,没有它就得另立一张按
  `volumeID` 键入的侧表,正是 D3.1 拒掉的那个"进程内 handle registry"。句柄必须带 `Volume`,否则
  `Release` 无从解析。
  - **不收 `accessMode`**:它既不改变 backend 做的任何事(三种模式下宿主目录完全相同),基数也错了
    (`AccessMode` 是 per-挂载行的,`Materialize` 是 per-卷的),故它搭 `MountSource.ReadOnly`——两者的
    规范陈述归 §5.C。
  - **契约是 create-if-absent**,不是 open-only(理由见 §5.C:源缺失时 runsc 硬失败,open-only 会让被误
    sweep 的沙箱永久无法 resume)。**并发性必须写进契约**——整个共享扇出设计依赖"同一个卷被多次并发
    物化"成立(已验)。
- `Release(m)` —— 释放这一次物化(local:no-op;remote:umount FUSE)。**名字不叫 `Unmount`**:那个词命名了
  默认臂**没有**的机制,而那正是一个 seam 长成某一个实现形状的方式。契约四条:这次物化的源不再有效、
  **字节存续**、幂等、容忍目标已消失——故 local 臂的 no-op 是对明确承诺的正确实现,不是坏味道
  (同 `runtime.Stop`);坏味道是"no-op 或 detach"那种一个名字配两份未定义契约。
- `AdoptMaterialized(volumeID) → Materialized` —— 崩溃后重认一个仍挂着的挂载点(§4.4-A,`AdoptMerged`
  的孪生)。
- `CreateVolume(ctx, volumeID, capBytes)` / `SetCapacity(ctx, volumeID, capBytes)` —— **容量属于创建 + 它
  自己的方法**,不搭在放置入口上。`rootfs.Provider` 需要一个 `CapUnchanged` 哨兵,是因为帽的意图搭在
  `OpenOverlay` 上(一个还兼着别的活的调用);这里**调用本身就是意图**,故不需要那个哨兵。且这是唯一
  正确的放法:**一个卷的帽在它没被任何地方挂载时也必须可编辑**,而只有放置入口表达不了这件事——
  `DestroyVolume` 本来也没有创建的对手方。
- `DestroyVolume(volumeID)` —— 回收该 Volume 的后端字节(terminal)。**幂等**(§4.4-A 从多条路径到达它)。
- `UncappedBytes(ctx) → int64` —— 本 backend 持有、且无任何已声明帽覆盖的字节(§3.4 第二项的 §5.C 半,
  与 §5.B 同形)。**这是真缺口不是装饰**:没有它,每一个无帽卷的字节对准入门都不可见。
- `Usage(ctx, volumeID) → (used, limit int64)` —— 单个卷的后端自述用量。**它是卷帽写点逼出来的,不是自选**:
  §5.C 要求拒绝"降到低于当前用量",判据是**严格不等式**,故引擎必须能读到当前用量;`inspect` 想报用量也走它。
  - **`limit` 只能用于诊断,不能用于校验,权威永远是卷记录**——理由(`none ↔ 0` 映射让"漏设帽"与"故意
    无帽"读回来逐字节相同,故事后核对不可实现)见 D7.3。
- `Snapshot(ctx, srcID, newID, capBytes)` / `Clone(...)` —— 归这个 seam,但**新卷的 ID 由 mediator 分配**
  (与 Promote 同规矩)。决定性理由:backend 自铸的 ID **无法在字节存在之前被登记进在飞集合**,故一次
  中途崩溃会留下一个任何 sweep 都无法归类的名字下的字节。
  - **两者收 `capBytes` 而不是"继承源的帽",因为没有可继承之物**,故契约里**设帽是不可省的第二次调用**
    ——非继承、必然是第二次调用、以及由此产生的那个承重窗口,全在 D7.3。
  - **两者可以是同一条 backend 代码路径**(实测:`property set ro false` 在本基底成功,`-r` 快照在仍只读时
    即可设帽,且帽完整穿过 ro→rw 转换)。差别只在 §5.C 的语义标注,不在机制。
- `OpenRoot(ctx, volumeID) → Root` —— 直连内容访问器,**未挂载也能用**(§5.C 的那条承诺,D5.1 定形)。
  `Root` 是一个**根限定**的文件访问句柄(`Stat` / `List` / `ReadAt` / `WriteAt` / `Mkdir` / `Remove` /
  `Rename` / `Close`),路径**相对该卷根**,越界不可达。local 臂 = `openat2(RESOLVE_IN_ROOT)`。
  - **它比 `Materialize` 更严,不更松**:句柄里**没有 `SourcePath`**、不吐任何宿主路径,故它是 seam 上
    唯一一个连豁免都不需要的物化类方法。
  - **它不是 `Materialize` 的替代**:那一条要交给 runsc 一个可 bind 的宿主路径,这一条只在引擎进程内读写
    字节。两个消费者不同(`local_runsc` vs orchestrator 的 import/export 端点),故是两个方法。
- `Name()` —— backend 标识(注册/诊断用)。
- `ListUnitIDs(ctx) → []string` —— **只枚举,不判定**。backend 报出它自己存储根下的全部存储单元 id;
  "哪些是孤儿"由 mediator 判定、并逐个 `DestroyVolume`。**这个切法是实测逼出来的,不是风格选择**——
  **否决"把判定也推给 backend"**(`SweepOrphanVolumes(live, building)`,mediator 算两个集合喂进来、
  backend 删差集):它**会删掉已提交卷的字节**。mediator 分两次读 `live` 与 `building`,一个**整个创建
  过程**落在两次读之间的卷两个集合都不在,backend 于是删了一个完整提交的卷。**实测:458 次并发 sweep 下
  6 个快照被删了 3 个;枚举-判定分离下 913 次 sweep / 6 个快照,零误回收。**
  - **它靠次序闭合窗口,不靠加锁**:每个候选的字节都早于全部检查;而一个已不在在飞集合里的候选,其行
    **必然已经插完**(标记只在插入之后才摘),故随后一次 per-candidate 的 store 读一定看得见它。不引新锁、
    不在锁内调 store。
  - **Overlay 侧故意保持合并形状**:那边的行**先于**单元插入,暴露的只有快照 skew,其注释把这段残留记为
    可接受;**卷是 bytes-first,暴露的是整个创建过程**——同一个形状,风险量级不同。两侧留了交叉引用。
  - **中途失败是继续、不是中止**(尽力回收路径,一个单元卡住不该让其余全部留着)。
  - **为何按单元而非按引用**:layer 是被 lineage 与 refcount **共享**的只读底座(判据是"还有没有行引用
    它"),而 Volume 和 Overlay 单元都是**自足的存储单元**(判据是"这个单元自己还有没有活的拥有者")。
  - **在飞集合在这里比 Overlay 那侧更必要**:Overlay 那边它只防**过期 keep-set**(行先插),而**卷的
    创建是 bytes-first**(ID 必须先命名那个单元),故一次健康的在飞创建**即便 keep-set 是新鲜的**也表现为
    "有字节、无行";`Snapshot`/`Clone` 又是同一窗口的另两个生产者。
- `ReclaimQuotaRows(ctx, ids) → (n int, err)` —— 回收**早先的删除泄漏掉的**配额行(D7.1 的那条路径的
  backend 半)。**收 id、不自己扫行**:扫描的失败形态是一个无帽的活卷 + 零信号(D7.2)。未到可回收态的行
  **留给下一轮**,不是错误——一个 `<squota space holder>` 可以在一个活克隆的整个生命期里合法地拒绝销毁。
- `DeletedRowIDs(ctx) → []uint64` —— 上一条要吃的那份输入:本 backend 在**删除那一刻**记下、尚未回收的
  配额行 id。**它落在 backend 而不是控制面上,是形状论据不是方便**:泄漏行是**该 substrate 的残留,不是引擎
  状态**——一个没有配额行的 backend 返空,整条路径对它零成本,故调用方不需要一个能力分支(§1.3)。
- `Capabilities() → {Snapshot, Clone, Quota}` —— **per-backend 能力查询**(D6):local(btrfs)报支持
  snapshot/clone/quota;纯目录或某些 remote 后端可不支持,调用即 400,而非静默。**成员必须是 `bool`**:
  一个 `map[string]string` 的特性表会把机制名当作**值**带过 seam,而任何扫标识符的结构守卫都看不见它。
  **成员只列有消费者的能力**(§1.3 禁止预留):`Quota` 有一个(§3.4 的帽兑现不了就拒 placement),
  而一个 `DirectIO` 之类的位没有——它会以"将来可能要"的名义永久留在契约面上。
  **作用域是 per-backend**,不是 per-volume。

**结构守卫**:`seam_test.go` 的孪生保留 §5.A 那份宿主原语清单原样,另加 `reflink` / `fuse` / `loopdev` /
`mountopt`;唯一豁免是句柄上的 `SourcePath`(照 `MergedPath` 的先例,连理由一起写在豁免表里)。

挂载进 guest 的机制:mediator 在 `coldStart` 里把每个 `Materialized` 句柄组装成 `LaunchSpec.Mounts
[]MountSource{SourcePath, Dest, ReadOnly}`(与 `RootfsSource` 平级的契约成员),`local_runsc` 在
`buildOCISpec` 里把每个 `MountSource` 追加成一条 `ociMount{Type:"bind", Source, Destination, Options}`
——正是实测过的形状。Volume **身份**在 seam 之上恒为 ID;宿主目录在 backend 内解析,只以不透明
`SourcePath` 浮出(§6.2 对外只认 Volume ID,不暴露宿主路径)。

---

## D3. 挂载表 = 一等 store 对象,复用既有对象模型(不重造)

挂载关系是 Binding(§3.4):"挂了哪些、挂到哪、什么策略"记在控制面挂载表,resume 时按表重挂。挂载表的
每一行是一个**一等持久对象**(§4.3 挂载行),它就是 **`sandboxes.template` 引用的翻倍**:

- **结构 = 双向 FK RESTRICT**:一条挂载行 `Mount{Sandbox, Volume, Path, Access}`,`sandbox` 与 `volume`
  两列各 `REFERENCES … ON DELETE RESTRICT`。这一条给了"卷有活挂载行则不可删"与"沙箱有活挂载行则删不动"
  两个不变量——SQLite 靠 FK、Memory 靠 Go 扫描,同一套断言(§4.3 seam)。
  - **"白拿"要分清哪一半**(实测):**检测谓词**可原样复用——既有的 FK-违反判定是表无关、列无关的字符串
    匹配,对新增的每条 FK 路径**两个方向**都为真(缺父插入 / RESTRICT 删除,三条 FK 共六向),对唯一违反
    为假。**但沙箱侧的翻译点今天不存在**——删沙箱的实现没有 FK 分支,RESTRICT 会以裸驱动错误冒到 API 的
    `default` 分支变成 **500 而不是 409**。且既有的 `ErrInUse` 文案字面写着 "template in use",被挂载行
    挡住的沙箱删除会返回一个说错对象的 409。故形状是**每个结局一个 sentinel**,而**哪个 sentinel 映到哪个
    结局类别、以及"数的是调用方要分辨的语义种类而非错误值个数"这条计数规则,归 §6.2**——本文只主张"不是
    复用一个"。Memory 后端更要**新写**沙箱侧与卷侧两处引用扫描 + 契约用例(既有契约清单里没有任何挂载
    形状的断言)。
  - **删除次序是必需的,不是风格选择**(实测):RESTRICT **立即**生效,把两条删除包进一个事务**买不到**
    次序无关性,故必须先删挂载行、再删沙箱。**`defer_foreign_keys` 是个陷阱,实测拒绝它**:它确实让次序
    无关,但违反在 `Commit()` 才浮现,而 Go 的 `Tx.Commit()` 返错时 SQLite **并未回滚** —— 在被钉死的单
    连接上事务保持打开,其后经连接池的查询**跑在该事务内部**、看见别的 handle 看不到的未提交状态。这是
    store 级正确性风险,不是风格问题。
  - **FK 强制本身是一条承重前提,且失去它是静默的**(实测:关掉 `foreign_keys` pragma 后删被引用的行
    直接成功、零错误)。故它值得一条契约测试守住,而不是靠 DSN 字符串的存在。
- **存在即状态、derived refcount、无 outbox 事件**:挂载行无 state 列、无 count 列;"卷是否被独占"由挂载行
  的存在+access 派生(不物化计数)。挂载行不是 §4.1 生命周期转换,故 create/delete 挂载行**不发事件**(与
  Template CRUD 同,区别于沙箱转换)。
- **bytes-first、转换锁内**:挂载行在**每沙箱转换锁**内、随 create/resume 冷启链写入;失败被 `unwind` 回滚。
  **`unwind` 必须显式删挂载行 —— 这是设计要求,不是实现细节**:挂载行写在冷启**之前**,故冷启之后的每个
  unwind 分支都会在删沙箱记录时撞上 RESTRICT;而一个丢弃该错误的 unwind 分支留下的是**一条删不掉的僵尸
  沙箱记录**。

**mediator 里的挂钩点**(动词归 §4,机制归 §5 backend;子系统间不互调):

| 钩子 | 动作 |
| --- | --- |
| `Create` | 记录插入后、`coldStart` 前写挂载行(纳入 `unwind` 回滚);`CreateSpec` 增 volume-binding 入参 |
| `coldStart`(create+resume 共用) | `Launch` 前:`Backend.Materialize` 每个卷 → 把句柄组进 `LaunchSpec.Mounts`;`liveState` 增 `mounts` 字段供 pause/destroy 释放 |
| `resumeLocked` | 从 store **读回**挂载表(挂载行跨 pause 持久)、重新 `Materialize`——与 `resolveLower` 每次重解析血缘同理,卷是同一真身、拿新的宿主物化 |
| `pauseLocked` | CAS 后:`Backend.Release` 每个卷(**挂载行保留**,租约不释放),对称于 Overlay「零捕获」 |
| `destroyLocked` | 卸载活卷 → **先删挂载行、再 `DeleteSandbox`**(否则 `mounts.sandbox` FK 挡住沙箱删除);这是租约释放点 |
| `RebindVolumes`(`PATCH /v1/sandboxes/{id}` 的 `volumes` 键) | 整份挂载绑定的替换:一次 `store.ReplaceMounts` 在**同一个提交**里删旧行 + 插新行,新行走**同一条租约谓词**;拒绝时旧绑定原样留下(原子性是 §5.C 写明的 store 契约) |
| 启动/周期对账 | **三个独立入口**,同一个启动位置与同一次 reaper tick 上依次调用:`SweepOrphanVolumes`(`Backend.ListUnitIDs` 枚举 → mediator 判定 → 逐个 `DestroyVolume`;**不**镜像 `SweepOrphanOverlays` 的合并形状,理由与实测见 D2)· `RepairVolumeCaps`(D7.3 的修复态)· `ReclaimQuotaRows`(D7.1 的泄漏行) |

**唯一的新不变量:独占租约**(规范陈述在 §5.C:至多一条 `exclusive` 行,且它不与该卷任何其他挂载行并存)。
这是 Volume 引入的唯一一条新 store 断言。**两个读起来自然、实测都不成立的陈述**,各值一条证伪:

1. **不是"一个卷至多一条 rw 挂载行"** —— 那与 SharedReadWrite 的扇出(§5.C)直接矛盾:shared-rw 同样是 rw。
2. **不是"控制面前置校验 + 插入,由单写者 store 序列化"** —— 单写者 store 串行化的是
   **每一次调用**,不是**跨两次调用的判定-动作**;而两个并发 `Create` 各自新铸 SandboxID、因此持两把**不
   相交**的转换锁,那一刻唯一被持有的锁是本沙箱的。§3.4 准入门是同一形状的另一例:门的不变量是节点
   全局的,而转换锁是 per-sandbox 的,故并发 create 之间根本不互相序列化,门因此另需一把独立互斥。
   实测:8 个 racer、无调度钩子、4000 次迭代里 2 次打破租约,加显式调度点则稳定复现。

**兑现方式:让 store 原子地做,不加应用层锁。** 一条语句表达完整的跨行谓词(含偏索引盲的全部四种组合):

```sql
INSERT INTO mounts(sandbox, volume, path, access)
SELECT ?1, ?2, ?3, ?4
WHERE NOT EXISTS (
  SELECT 1 FROM mounts m
  WHERE m.volume = ?2
    AND (?4 = 'exclusive' OR m.access = 'exclusive')
)
```

`RowsAffected()==0` ⟹ 租约冲突(→ 409)。实测 24 个 racer、不加任何应用锁:rows=1 / exclusive=1 / 零驱动
错误;多卷 all-or-nothing 用一个事务(单连接使其天然串行)测得 winners=1/16、partial=0。**推荐它而不是新加
一个 per-volume 锁类**,理由是不引入新机制:它就是 `store.Store` 本来就要求每个后端交付的事,且与 §4.3
引用-回收**同形**(同一个权威 store 串行仲裁判定-动作)。Memory 侧是其既有互斥下的一个方法。偏索引与
`CHECK (access IN (...))` 退为 schema 级断言(它们各自只覆盖一半,见 §5.C)。

> **竞态写者只有两个,且都是挂载行的写**:`Create` 冷启链上的 INSERT,与 `RebindVolumes` 经
> `store.ReplaceMounts` 的删+插(同一个提交、同一条租约谓词,故它不是第二套仲裁,是同一条谓词的第二个
> 调用点)。这把修法收窄了很多:resume 只**读**该表,destroy 只删自己沙箱的行且 FK RESTRICT 覆盖次序,
> pause 不写行。
>
> **谓词必须是行内不可变的属性**(§5.C 的禁令)有一个具体的外部反面教材:某实现的 live-owner 偏唯一索引
> 建在一个软删时间戳上(`UNIQUE(...) WHERE
> cleanup_requested_at IS NULL`),写入它的路径存在、而**全代码库没有任何路径把它清回 NULL**——于是一次误标
> 既永久判死该卷,又腾出唯一性槽位让第二个持有者插进来。我们的谓词是 `access`(行内不可变),这正是躲开该
> 失败模式的原因;**它是一个选择,不是巧合**,故写下来,否则下一个人很容易把它改成某个 `detached_at`。
>
> **锁序(全序)**:`transitionLock(sandbox) → mediator 的 `mu`(live-map 与直连写的 in-flight 标记同住)`,
> 以及 `transitionLock(sandbox) → 准入互斥`。**没有 per-volume 锁类,这是刻意的**:卷维度的互斥由上面那条
> store 语句承担(D3 的整个论点),而写入 regime 的两半共用 live-map 那把 `mu`,故 Volume **不新增锁层**。
> `mu` 严格是叶子(取了就放,其间不调 store、不调 backend、不等别的锁);**准入互斥不是**——它按契约横跨
> {求和 → 物化 → 提交},持有期间会等 store 的单写者锁。两者仍可组合,因为**没有任何 store / backend 路径
> 反向去取这两把锁**,且它们彼此不嵌套。
> **这同时是把抢占留在门外的一条独立论据**:一个卷维度的动词要去反查它的持有者(force-detach / 租约抢占,
> 正是 D8 刻意推迟的那条)就得引入一把 vol→sbx 方向的锁,与 `transitionLock → …` 闭合成环。**这一条是从
> 锁序推出的,repo 里没有它的复现载体**(与 D3 前两条不同——那两条的数字在 `store.Store.CreateMounts`
> 的注释里)。若它将来要落地,必须从 store 解析出持有者、**先释放、再按全局序重新获取**。

### D3.1 证伪:store-级独占 token vs 进程内 open-handle refcount,不可混

一个诱人的备选是保留**两层** refcount:① DB 挂载行(谁独占、跨进程)② 一个进程内计数(本 node 上多少个
挂载共享**同一个已打开的 backend 会话**)。第②层只在"一个 FUSE 会话服务 N 个沙箱"时才有意义——它数的是
**打开的合成 FS 句柄**的引用数。本引擎 local 后端是"gofer 各自代理一个宿主目录",**没有共享的 open
handle**,故第②层退化掉,只剩第①层(挂载行)。第②层因此是 **remote/合成 backend 的内部实现细节**,不进核心
挂载表模型——记此以免把分布式的双层 refcount 误当核心需求。

---

## D4. 访问模式与独占租约

三种访问模式的定义与语义、以及"独占租约 = 那条 Exclusive 挂载行的存在本身"(不引入 `Lease` 类型、
不加归属字段;含"崩溃保持租约是正确行为"与 reaper TTL 兜底)的规范陈述归 §5.C。这里只留两点:
命名取"共享 + 可写性"的**可观测属性**(§1.1 词汇边界禁用归属义的实体名),以及 SharedReadWrite 的
"跨沙箱 rw coherence 不定义"与 D1.2 拒 FUSE-统一缓存**同源**——跨边界共享可变状态是难题,回应是
**不承诺**,而非造脆弱机制假装承诺。本节其余篇幅给它排除掉的那条备选。

**证伪:心跳失效 + Pod 销毁双回收。** 它是**分布式无单一权威 store、node 会凭空消失**的产物(node 死了没人
知道,只能靠心跳超时推断):注册-心跳 + "按心跳超时查活挂载"是它的标准形状。
本引擎单机单 orchestrator、store 即权威(§1.1):没有"凭空消失的 node",崩溃是 L1 探活立即可判的本地事实。
故**留 reaper TTL 兜底、去掉周期心跳循环**(没有可交接的另一 node)。心跳式回收是未来 multi-node provider
的义务(node 死亡由其调度层接),不进核心。**唯一真残留**:长期 paused / 卡住的沙箱占着独占卷、他方现在
就想要——引擎默认独占直到 destroy/evict,不主动抢占;要不要强制 detach / 抢占是策略(大概率产品层),
登记 open-decisions(D8)。

---

## D5. 写入 regime:一条不变量,不要反向代理

hazard 是真的:卷可能同时被运行中沙箱经 gofer rw 写、被直连 API 从宿主侧写同一后端 → 撕裂,且 gofer/sentry
页缓存与直连写不 coherent。§5.C 用一条**不变量**堵掉它——该不变量(live-rw 挂载在持 ⟹ 直连变更一律拒;
门开在"**活的**"写入方,`paused` 不算 live writer)的规范陈述归 §5.C;本文只给它的边界与证伪。

**未挂载时的直连操作**(create/delete/list/inspect/import/export):local 后端直接对宿主目录/subvolume 操作,
单机场景简单,无需"惰性 attach"。"无挂载时临时开一个 backend 会话服务直连、闲置回收"这套是**合成型
backend 才需要的**(要一个打开的 FS 会话去服务),local 目录用不上——封在 remote backend 内。

### D5.1 直连访问器的形状:`openat2(RESOLVE_IN_ROOT)`,不经 guest、不用 chroot

直连 import/export 是一个**窄访问器**,落在 `volume.Backend` 上(`OpenRoot`,D2),不经 §5.E `fs`。
两条论据:

1. **经 §5.E 结构上不可行**(已核对代码):`fs` 只存在于 guest 内、只能经一个**活 placement** 到达,
   而未挂载的卷没有 guest;且把卷内容路由经 §5.E 会产生一条 `volume → transport` 的子系统边,
   `internal/arch` 的空 allowlist 当场失败。
2. **窄访问器的安全边界只要一个 syscall。** `openat2(dirfd, path, RESOLVE_IN_ROOT)` 给出与 chroot
   **逐条相同**的路径语义,且无特权、无 mount namespace、无线程钉定。(实现同时带 `RESOLVE_NO_MAGICLINKS`
   ——它独立堵掉经 `/proc/*/fd` 魔法链接的逃逸,**不改变**下面这四条路径语义,故是加固而非另一条边界。)
   实测四条(本机 6.x 内核):
   `..` 在根处**夹住**(不是报错,是解析成根本身)· 绝对路径**按该根重新解释** · 指向根外的 symlink
   (相对与绝对都试过)解析回根内 · symlink 环报 `ELOOP`。写侧同验:经一条 `../../../d/f` 形状的路径
   create+write,字节落在卷内;一条指向宿主文件的 symlink 上的写**没有**碰到宿主文件。
   - **量级**:`openat2(RESOLVE_IN_ROOT)` **0.52 µs/op**;一个参考实现为同一个保证付的
     "每请求一个新 mount namespace + bind + `pivot_root`" 是 **178 µs/op**(同机实测,后者还要 root)。
     **342×**,且省掉整套 namespace 生命周期与 `runtime.LockOSThread` 的线程所有权协议。
   - 故"直连访问"不是一个需要新数据面的能力,而是**一个 dirfd 加一族 `*at` 调用**。它连
     `SourcePath` 都不必吐:句柄不暴露任何宿主路径,比既有 `Materialized` 更严,`seam_test.go`
     的结构守卫自动通过。
   - **remote 臂容得下**:FUSE 挂点是目录,`openat2` 在它上面逐字相同;真正的对象存储后端则自己实现同一
     组路径+字节语义(接口是路径与字节,不是宿主原语)。

访问器**是写入 regime 不变量与它的 in-flight 标记唯一的触发者**,故三者同门落地——单独实现后两者就是
§1.3 禁止的无消费者契约成员。

**写入 regime 的门不能退化成"挂载表里有行就拒"。** 那样更保守、不会撕裂,但会毁掉 §5.C 明写的承诺——
`paused` 沙箱不算 live writer,此时直连写安全且 resume 后自然见到新内容。而 live-map **确实是**忠实的
gofer 谓词:已核对代码,placement 在**物理存在的那一刻**就被 `put` 进 live-map(在健康门**之前**,
`lifecycle.go` 的 `coldStart` ⑦ 前一步),正是为了让并发 reconcile 不把它当孤儿。故谓词读 live-map
是对的,`sandboxes.state` 是错的(§5.C 已记两向不忠实),而"只读挂载表"是**第三种**错——它把一个已定的
能力换成了实现的方便。

**证伪:直连反向代理到挂载沙箱**(卷挂着时把直连写反代进持有沙箱,以允许"挂着也能从外面写")。**不采用**:
①这是**产品便利**、不是引擎正确性需求——引擎只要"别撕裂",§5.C 的**拒绝**就够,成本远低于造一条反向代理
数据面;②反向代理要引擎理解"谁在挂、如何穿进它的 gofer 写它的 FS",把**跨对象写入编排**拉进引擎(§1.3
litmus:非 register-impl / add-join-table / add-subscriber,而是改现有结构 → 该问"是不是不该进引擎")。
答案:要这个便利就叠一层引擎外 adapter(读挂载表 → 经 §5.E `fs` 写进持有沙箱),引擎不做。产品平台把这层
便利算进自己职责是合理的;引擎划在边界外,这是**定位差异,不是对错**。

---

## D6. 快照 / 克隆:btrfs 原生 CoW,per-backend capability

### D6.1 单机用 btrfs,不造用户态 segment 引擎

"POSIX over 对象存储"要一整台机器:WAL(node-local 变更日志)→ 周期物化成不可变
manifest + 内容寻址 segment → head 指针 CAS,快照=引用同一批不可变 segment、fork=改写 segment 引用做 CoW。
**这台机器整体属 remote backend 的内部义务,不进核心**:它存在的唯一理由是"对象存储不是 POSIX、
无 durability-on-write、无 reflink、不可信"。单机 btrfs 上,**文件系统本身就给了 CoW**——`cp --reflink=always` 已验证共享
extent(同一物理 offset、`shared` 标志,即便 `nodatacow` 挂载下显式 reflink 仍生效),而 `btrfs subvolume
snapshot` 更是**整树、原子、瞬时**(实测新快照仅 16KiB exclusive 元数据、零数据拷贝)。

所以:**采纳语义模型**(快照=不可变时间点、fork=CoW 子卷、二者元数据级廉价)与**能力矩阵**(快照/fork 是
per-backend capability),**丢弃 segment/manifest 实现**——让 btrfs 当"materializer"。若把 Volume 建模成一个
**btrfs subvolume**,快照/克隆 = `btrfs subvolume snapshot`,与 D7 的容量帽(配额打在 subvolume 上)
落在**同一个对象**上:同一个存储单元同时得到廉价快照与硬容量帽,这是"卷=subvolume"的强理由。**但别把它读成
"一步到位"**:那个 ioctl 搬的是**字节**、不搬**帽**(D7.3),所以这个合并是两次操作而非一次。

### D6.1a 快照 / 克隆的对象形状:同一种对象,带 lineage,帽必须显式声明

三个子决定,合起来定义"快照是什么":

- **快照卷是一个普通的、可写的 Volume,不加只读标记。** 理由与 §5.C 拒掉"per-卷 `access` 字段"逐字同构:
  **基数错了**(可写性是 per-挂载的裁决,一个卷可同时对 A 是 shared-rw、对 C 是 shared-ro)。更硬的一条是
  §5.C 已实测并写下的性质——"快照不破坏 per-volume 帽语义:**写入方付账**,两个方向都是",覆写快照只记快照账、
  源不动。那条实测**只在快照可写时才有意义**;把快照定为只读等于把已验证的性质丢掉,还要给挂载行 `access`
  加一条跨对象校验(§1.3 litmus 上过不去:既非注册实现、也非加联表,而是加字段 + 加跨对象检查)。要不可变
  就用 `shared-ro` 挂载行表达,或留给产品层策略。
  - 连带**收束了机制**:若快照是独立的只读类型,就要两个 seam 方法、两套 API 形状、两条拆除规则,去对应
    btrfs 用**一条**相同 ioctl 实现的东西。同一形状让 `Snapshot` 与 `Clone` 共用一条路径(D2)。
- **卷记录带 `parent`,`REFERENCES volumes(id) ON DELETE RESTRICT`**,与 `templates.parent`(§4.3 lineage)
  一模一样。这是**正确性论据、不是整洁**:D7.1 实测出"先删共享者、再删源,一行不漏",而 FK 是让那个次序成为
  **唯一可达的次序**的东西——删一个有活快照的源变成 409,配额行泄漏**根本不产生**(预防,免费,不引新机制)。
  按本引擎自己的判据它干净通过:它就是 §1.3 的"加一张联表"(此处是自引用)、复用既有的 FK-违反判定谓词
  (D3 实测其表无关列无关)、无 state 列无物化计数("这个卷是不是源"由有没有行指向它派生)。
  - **外部反面教材把这条钉死了**(已核对源码):某实现选了**有指针、无约束**(`ADD COLUMN ... TEXT` + 索引,
    无 `REFERENCES`),于是不得不在三个独立位置手写 children 检查——而那三处**全是回收守卫、没有一处是删除
    守卫**。后果是控制面行先消失、字节还得留着,而所有回收判据都键在源卷自己那条记录上,于是源行一删,
    **任何后来的 pass 都再也找不到那些字节**(它的"若有子代则跳过回收"函数只在同步删除路径上被调用一次,
    没有任何周期性 sweep 回访)。我们的 `SweepOrphanVolumes` 能覆盖这种情形,但**不该需要它**:RESTRICT
    让这个局面根本不出现。
  - **需要自己的 sentinel**:被活快照挡住的卷删除与被挂载行挡住的卷删除是**两个不同的对象关系**,而既有
    `ErrInUse` 的文案写着 "template in use"。这是 D3 已经点出的同一个陷阱在第四条 FK 路径上的复现。
- **`size` 在 disk 被计量时必填、不继承**,故 `POST /v1/volumes/{id}/snapshot` 与 `.../clone` 都长出请求体
  `{size}`。**基底侧的选择权已经没有了**(D7.3:没有可继承之物);而在策略侧,继承看着免费其实不是——帽是
  **声明的准入主张**,不是字节的属性,§3.4 正因为快照的元数据级实际字节约 16 KiB 才规定"按声明帽计"。继承会
  在每次快照时静默把节点的已声明磁盘主张翻一倍,而调用方从没提出过;且 §5.C 已写明 squota 的帽是"**终身写入
  预算**",一个还没写过任何东西的单元继承一个数字毫无意义。要它必填也让规则数不变:§6.2 已有"按轴条件必填",
  这只是同一条规则出现在第二、第三个端点。源无帽且该轴未计量时 `size` 照旧可省,由既有规则自然覆盖。

### D6.2 快照默认 crash-consistent,与 Overlay 不对称

Volume 快照默认 **crash-consistent**、与 Overlay 不对称的完整陈述在 §5.C(含 app-consistent 的组合路径:
独占卷由调用方先 pause 再快照,shared-rw 不提供)。此处只补两条本文该留的:不对称的**根源**是 Overlay
**单一写入方**、可 pause 到 at-rest 读(§5.B),而 Volume 打破此前提(shared-rw 多写入方、挂载中不想停);
以及**为什么快照前既不 `fsync` 也不 freeze**(一个诱人的"稳妥起见"前置动作:sync 那半无用、freeze 那半净负):

- **sync 无可测差别**:sync 后的快照与裸快照落在同一个一致性类(每次捕获都是完整前缀),裸快照本就
  **不卡 guest**(零写入错误、写入速率不变)。
- **`fsfreeze` 与快照根本无法组合。** 创建一个快照本身就是对你刚静止的那个文件系统的**写**,故快照在整个
  frozen 窗口内**不可中断地阻塞**、只在 thaw 之后才落地——它因此排到了它本想利用的那次静止**之后**,捕获
  的是 thaw 后的状态,相对裸快照**零一致性收益**;唯一的实际效果是把 guest 卡住,外加冻住整个节点的根
  文件系统(单机上卷与引擎状态常在同一个 btrfs 上)。
- **连带的 harness 约束**:不要用 `timeout` 包住 frozen 窗口里的快照——一个被信号打断的阻塞快照会**破坏
  freeze 计数**,其后的 thaw 返回 `EINVAL` 像是已解冻而快照照样落地(4/4 复现)。踩上它会把测量工具制造的
  假失败("快照失败 + thaw 失败")读成基底失败。

### D6.3 证伪:二段式 checkpoint

用"阻止新操作 + 排空飞行事务 + 强制刷盘"的二段式给挂载中卷拍一致快照——**不采用**,两头不落好:①对独占卷
冗余(二段式本质就是 §4.2 的 pause 静止点,已被"pause 持有沙箱再快照"覆盖,再造一份会让 §5.C 反向依赖
§4.2,违反子系统间不互调);②对 shared-rw 不够(N 个独立沙箱的 gofer 无共同协调者,强造一个就是把 swarm
编排塞进引擎,§1.3 否决)。

---

## D7. 容量配额:btrfs subvolume + squota,跨 §5.B/§5.C 共享原语

机制(btrfs subvolume + squota limit)、模式选择(squota 而非完整 qgroup,及本基底改不了模式)、全栈实测
与归属(叶子包 `internal/btrfs`,**共享的是代码不是 seam**)**都在 rootfs-overlay D6 讲清,本文不复制**
——§5.C 的 per-volume 帽与 §5.B 的 per-sandbox upper 帽是同一个物化问题的两处应用(§3.4 容量轴),Volume
**复用同一个包,不重造、不改其形状**。

本节只留 Volume **自己引入**的三件事(Overlay 侧不存在,因为引擎从不快照 upper):**配额行会泄漏且是正确性
bug**(D7.1)· **一次误 sweep 的爆炸半径**——它解释了 §5.C 每条 sweep 要求为何都是硬要求(D7.2)·
**limit 侧:没有继承、漏设不可检出、降帽不可逆**,它定了两处契约形状(D7.3)。

两条直接后果先记在此:

- **snapshot/clone 出来的卷,其 reflink 字节在 squota 下不向新卷计费**——快照廉价这一性质因此在配额语义上
  也成立;但"一个卷占多少真实磁盘"要从节点级核算看、不能只看单卷配额(与 rootfs-overlay D6.2 里 Template
  layer 字节同源的问题)。
- **本引擎不是该盘配额命名空间的唯一所有者**(实测该 fs 上有 11 行外部 qgroup,OrbStack 也用;嵌套 subvol
  实测共存无碍)——这正是 §5.C「配额行按删除时记下的 id 定位,**绝不扫行**」的前提。

### D7.1 Volume 带来的新问题:squota 的配额行会泄漏,且它是正确性 bug

§5.B 的 Overlay upper **今天不泄漏**,原因是引擎从不快照它;§5.C 要快照卷,故这是 Volume 引入的新问题。
**泄漏的谓词不是"被快照过"**,而是:**一行泄漏 ⟺ 该单元 (i) 拥有 extent 且 (ii) 在仍有活共享者引用这些
extent 时被删除**。实测矩阵的两条推论:

- **有没有帽与泄漏无关**(无帽的源同样漏);**被快照过但为空的源不漏**(它不拥有 extent);链中的**中间**
  节点写过数据、且先删它,也漏(它不是链根)。
- **先删共享者、再删源,一行都不漏。** 故**拆除次序是一条真的杠杆**,不只是缓解手段——它把回收从"清理"
  变成"预防",且免费。

**为什么是正确性 bug 而不是不整洁**:泄漏行本身无性能后果(60 行时枚举仍是毫秒级、无行数上限),但
**next-free subvolume id 在挂载时从 root tree 重算,而配额树没有发言权——一行泄漏不为自己的 id 占位**。
故一次 remount(节点重启)可以把某个泄漏行的 id 连着**它的帽**发给一个全新的存储单元:实测一个从未被设过
任何 limit 的新单元在 3.3 MB 处撞 `ENOSPC`(另一形状:20 MiB 写入死在 11.8 MiB)。方向也可能相反(泄漏行
带着更大的帽 ⇒ 静默超额供给),且**引擎内部无法察觉**——它读回的 limit 看起来就是这个单元自己的。
可达性依形状而定、不普适,但既然没有任何东西为那个 id 占位,**它不能被论证掉**。

> **`parent` + FK RESTRICT(D6.1a)把这条路从"常规"降为"backstop"。** 泄漏的必要条件是"删源时仍有活共享者",
> 而 RESTRICT 让那次删除**在 API 上根本不可达**(→ 409)。剩下的可达路径只有两条,都不经用户意图:一次
> **崩溃**卡在删共享者与删源之间,以及 sweep 自己按单元回收时的次序。故下面这个程序仍然必须存在、必须正确,
> 但它守的是残余而不是主线——这也是为什么它的错误处置是"跳过并留到下轮",而不是"阻塞"。

**回收程序**(§5.C 要求的那条路径,mediator 侧归 `ReclaimQuotaRows`)。它与 `SweepOrphanVolumes`(回收
孤儿字节)、`RepairVolumeCaps`(修 D7.3 的那个修复态)是**三个互相独立的入口**,只是在同一个启动位置与
同一次 reaper tick 上被依次调用——**别把它们读成一个 sweep 的三个阶段**,三条残留的谓词、失败处置与
可达路径都不同。每轮:

1. **先删共享者(快照/克隆)、再删源** —— 按上面的矩阵,该次序一行不漏。
2. **每轮一次** `filesystem sync` 然后 `subvolume sync`(不是每单元一次)。次序是实用发现:
   `subvolume sync` 单独用会**睡到下一次周期性 commit**,故它的耗时随 commit 周期与负载浮动、**不是一个固定属性**
   (多次实测落在数秒到十几秒:7–16 s、19.1 s);前面先 `filesystem sync` 强制那次 commit,两者合起来在**几十毫秒**
   量级返回(实测 14–29 ms、53 ms)。**承重的是这个量级差(约三个数量级),不是任一具体区间**:效果相同,而从
   "等一次 commit"变成"有界"。
3. 对本轮删掉的每个 id 读它的配额行,**先查退出码**:读失败 → **未知**,不得据此下结论;无行 → 完成;
   **`<stale>` → destroy**(轮询谓词就是"标签恰为 `<stale>`");`<under deletion>` → 再 settle 后重试;
   `<squota space holder>`(有活共享者)→ **留着**、下轮再说。
4. **轮内重试上界 3 轮**,超界则留行 + warn(带 id 与标签)、下轮重试。**绝不在 EBUSY 上死转** ——
   一个 `<squota space holder>` 可以在一个活克隆的**整个生命期**里合法地无法 destroy,故该上界不是错误路径。

> **转变是被条件触发的,不是被时间触发的**:共享者一死、删除一 settle,**第一次**尝试即成功;故必须轮询
> 谓词,不能 sleep 后祈祷。活单元的标签是它的路径、每个可回收态都是 `<...>` 标记,故该谓词**不可能在运行中
> 的单元上误触发**(另测确认:destroy 一个活单元的行返 EBUSY,且其帽强制与用量可读性不受影响)。
>
> **一个会咬人的读取陷阱**:配额行的批量读取在并发 churn 下会**整体失败、一行都不返回**(它为每一行解析
> 路径,而其中一个在脚下消失了;单路径形式同样会,实测频率约 1/150 —— 而那正是 sweep 运行时的条件)。
> **把空输出当成"行已消失"的读取方会静默跳过一次 destroy,那是永久泄漏。**
>
> **一条同族的未来陷阱(值得在 §5.B 侧留注释)**:真正的触发条件是 **extent 共享**,不是"做过 snapshot"。
> 实测 reflink **进来**(copy-up 的形状)不泄漏,reflink **出去**会泄漏。故一个显而易见的未来优化——让
> Promote 用 `cp --reflink` 替换逐字节拷贝——会**每次 Promote 漏一行**,且该泄漏比那个 layer 活得更久。

### D7.2 一次误 sweep 的爆炸半径:为什么 §5.C 的每条 sweep 要求都是硬要求

实测在一个**持续读写**的活 guest 底下用五种手法拿掉它的卷:**五种全部只是数据面事件**——沙箱一次都没死
(容器仍 running、新起的 exec rc=0、runtime 无错误),故**控制面什么都看不到**,与已测的 ENOSPC 情形结构
同形。但推论比"guest 会见 I/O 错误"这个假想的坏情况**更糟**:

| 手法 | guest 见到 | 数据结局 |
| --- | --- | --- |
| 删掉存储单元 | 已解析过的路径**永久照用**;只有新的 lookup 报 `ENOENT`(不是 EIO、不是 ESTALE) | 写进 unlinked inode,**静默丢失** |
| 删掉单元内容 | **零错误**;guest 立刻重建自己的文件 | **静默丢失,毫无信号** |
| unlink 一个打开的文件 | **零错误**;经旧 fd 的读写照旧 | 经 gofer 兑现了 POSIX;fsync 不复活宿主路径 |
| 清掉配额行的 limit | **帽被静默解除**:先在帽上撞 ENOSPC,随后写入恢复并到达 2.5 倍帽 | §3.4 记账被破,**无任何信号** |
| 把单元 `mv` 走 | **零错误**;从不看见留在老路径上的诱饵 | gofer **跟着 inode 走**,其后的写落进被挪走的目录 |

**五臂中有三臂一个 errno 都不产生**,故"guest 会报错"不是检测路径。三条连带的硬事实:

- **`limit none` 是唯一一个破坏了数据完整性以外的引擎承诺的操作**,而它**没有 EBUSY 自保**(destroy 在活
  单元上有:返 EBUSY)。这正是 §5.C 要求"配额行按删除时记下的 id 定位、绝不扫行"的原因——扫行的失败形态
  是一个无帽的活卷 + 零信号。
- **误 sweep 一个活单元连它想回收的空间都拿不到**:删除只是**排队**(gofer 持着 fd,btrfs 就回收不了
  extent),而**配额行立即消失**——即记账先消失、字节还在,故"sweep 回收了 N 字节"在 sweep 时不可观测。
- **同一个沙箱里两个进程会对"卷还能不能用"给出不同答案**:新进程没有缓存的 dentry,故在老进程仍写得通的
  地方得 `ENOENT`。
- **恢复是"可恢复但数据全失"**,且这个结论**依赖 `Materialize` 是 create-if-absent**:源不存在时 runsc
  硬失败、不会静默建目录;把单元重新建成空的则干净重启。

### D7.3 帽的 limit 侧:没有继承,漏设不可检出,降帽不可逆

D7 以上各节讲的都是**用量**侧(哪些字节记给谁)。**limit 侧是另一半,且它的结论决定了两处契约形状**
(D2 的 `Snapshot`/`Clone` 签名、§5.C 的卷帽写点)。三条实测:

- **没有任何继承。** 快照得到一条**全新的、`max_referenced = none`** 的配额行,四种形状皆然(rw 快照 /
  `-r` 只读快照 / 无帽源的快照 / 给快照设帽后再快照它)。且是**写入验证过的**,不是读数:往无帽快照灌
  96 MiB(源帽的 3 倍)全部落盘,而同一形状下显式设了帽的负向对照在帽上精确失败——故 `none` 是真的没有帽,
  不是读错。两个方向的隔离也确认:往快照灌 96 MiB 期间源的 `referenced` 一动没动;源钉死在帽上时往快照写照旧成功。
  - **`btrfs subvolume snapshot -i <qgroupid>` 看着像答案,不是。** squota 下它被接受,但它设的是**成员关系**
    不是帽:新单元自己的行仍是 `none`,而 level-1 父上的 limit **确实强制**。也就是说它是**所有成员共享的
    天花板**,表达不了"这个卷得 N",且一个成员耗尽会让**兄弟卷**一起 `ENOSPC`。它在单卷测试里会像能用,
    在生产里炸邻居——故明确排除。
- **漏设帽是静默的、事后不可检出的。** 一个"快照了、记录也写了 `size=N`、但忘了设帽"的引擎:写入方可以灌
  远超 N 的字节,零错误、无生命周期事件、控制面一无所知。更糟的是 `Usage()` 读回的 `limit=0` 与**故意无帽**
  的卷**逐字节相同**(`internal/btrfs` 两个方向上都把 `none` ↔ `0` 映射),所以没有任何对账、sweep 或
  inspect 能在事后发现这个分歧。**这把不变量逼到写时**:设帽必须是创建字节的那次 backend 操作的一部分。
  - 它是 D7.1 的**镜像,且严格更坏**:D7.1 要靠 remount 落在特定 id 上才触发(形状依赖),而漏设帽
    **每次快照都触发、无前置条件**。
  - 由此产生一个**承重的崩溃窗口**:帽必然是快照 ioctl 之后的第二次调用,故"快照已存在、帽还没设"是一个
    真实中间态。在飞集合(`building`)必须覆盖它,且 `RepairVolumeCaps`(以及 adopt 路径)要把"卷记录存在、
    配额行是 `none`"当作**修复态**而不是合法态——否则一次崩溃留下的就是一个已登记、活着、无帽的卷。
    它是**自己的入口**,不是 `SweepOrphanVolumes` 里的一个分支(D7.1 的三入口)。
- **降帽到低于当前用量会把单元卡死,且不可逆。** 基底接受这次降帽且**零校验**;既有字节安然无恙(读回逐字节
  相同);但其后 `mkdir` / 建文件 / **把已有文件 truncate 成 0** 全返 `ENOSPC`(只有 unlink 还活着),而且
  **配额 ioctl 自己也返 ENOSPC**——帽既不能撤、也不能抬,**连抬到高于当前用量都不行**。唯一出路是删字节删到
  用量落回帽下(条件触发,不是时间触发:逐级降低用量验证过,只要还在帽上就一直拒)。边界情形:`limit == 用量`
  被接受并存下,但随后一次写就 `ENOSPC`——故它意思是**满**,不是"刚好够"。
  - **故卷帽的写点必须拒绝这种降帽(400),判据是严格不等式 `size > 当前用量`。** 理由不是基底禁止(它乐于
    接受),而是**接受会造出一个控制面出不来的状态**:修复要经由删 guest 的字节,而 §6 里没有任何东西允许
    引擎代调用方做这件事。§3.4 的"降帽不需要重过准入门"因此配了一条 caveat。

### D7.4 guest 侧 statfs 为什么不反映帽

用户可见语义(含实测数字与"能诚实投影的后端"那条义务)在 §5.C;本节只留它的根因与外部佐证。

**根因**:btrfs squota **没有 statfs 投影**,故 guest 看到的是宿主整盘数字的**忠实透传**——不是 sentry
fabrication(`f_type` 是 gofer 的而非 btrfs 的,即这条数字根本没经过帽)。这与 §5.B 的"guest 侧数字不可用于
容量核算"是同一条规则的另一个方向,但它是 **guest 可观测的意外**。**一个独立的外部实现踩了同一个坑**
(其挂载卷上报约 4.6 EB 可用,成因不同——疑似网络 FS 代理——症状同类),故这不是本基底的偶然。

故本节挣到一条比"不承诺 statfs 反映帽"更强的陈述:**在 squota 上它确定不反映**——这不是未承诺,是已知为假。

---

## D8. 契约边界外待办(登记 [`open-decisions.md`](../open-decisions.md),不在本文拍板)

以下是设计已定、但**具体取值 / 落地形态未定**的边界项(产品层策略,或落 §6 时定):

- **独占卷的抢占 / 强制 detach 策略**(D4):默认独占直到 destroy/evict;要不要 admin 强制 detach、抢占
  优先级——策略,大概率产品层。
- **直连变更 vs live-ro reader 的准入细节**(D5 边界):有 live-**ro** 挂载时允许直连写会让 ro reader 的
  gofer 缓存与后端不 coherent;要不要加告警 / 版本戳 / 拒绝,精度问题。
- **snapshot 的一致性标注如何进 §6 契约**(D6):API 是否显式标注"本快照 crash-consistent"——落 §6 时定。
  (只剩标注的措辞;"继承 quota"不是开放项,`size` 必填见 D6.1a / D7.3。)
- **远端 backend 的落地形态**(D1/D2/D6):S3 / NFS backend 何时、以何形状落;其内部(host-FUSE 前端、
  完整 FUSE 会话的 POSIX 接口、以及若接对象存储则 WAL/manifest/segment/内容寻址/加密/GC 那一整套)
  **全封在该 backend 内**,不进核心——本文只确保 seam 形状容得下它。**D2 的 handle 形状是按"宿主 FUSE
  挂点能经 gofer 代理"这一条已验事实推理得出的,FUSE 臂只实测了读/枚举穿透(D0),写路径与真实远端
  后端未测**。
  - 一条对**任何**未来 remote 臂成立的教训(来自一个分布式参考实现的失效,机制不用抄——我们单机、store
    即权威、无 split-brain):**让字节在所有权声明提交之前不可达,且绝不让两个 epoch 共用一个 key**。
    放在写之后的 CAS 拦不住两个写者算出逐字节相同的 key;而"payload 里存了校验和"救不了它,因为同一个
    写者会把两边都改成互相自洽。

(直连 import/export 的 seam 归属**不在本表**:它已定形,见 D5.1。)

---

## D9. 三条与参考实现的分歧

1. **按 ID 寻址,不按 name。** 一个参考实现的挂载项**按卷 name 寻址**,且其 SDK 手里握着完整卷对象(含 ID)
   时仍然只发 name。这与 §6.2「对外一律 Volume ID 寻址」正面相左,而我们有独立理由(§3.1.1 对内也 ID 寻址
   是同一条规则的内部延伸)。**保持 ID 寻址。**
2. **我们有而两个参考实现都没有的三项**:只读挂载 / 任何形式的独占或租约 / per-volume 容量帽。特别是容量帽
   ——两个实现都没有任何字节上限字段,其中一个的容量甚至是**虚拟的**(`statfs` 硬编码 4 TiB,文档明说那"不是
   存储分配、不是硬配额、也不是节点文件系统容量"),配额只在 team 级、且是**事后测量**;而我们实测 btrfs
   squota 兑现的是**精确到帽的硬 `ENOSPC`**(精度 ~0.05%,rootfs-overlay D6.2)。故 D8 的这几条**没有外部压力
   要求砍掉**——承重的是"硬配额 vs team 级事后测量"这个性质差,不是精度的具体值。
   > ⚠ **那个精度数字的效力**:它出自 D6.2 的一次性实测,**没有脚本打印它**;而唯一在断言 ENOSPC 精度的
   > `spike-volume-cap-snapshot.sh` 用的是**远宽于它的容差**(8 MiB 帽上 ±2 MiB)。故 `~0.05%` 可引作
   > "落点紧贴帽"的量级证据,**不可引作一个可复现的精度指标**。
   - **快照/克隆不在这张缺口清单上**:其中一个实现两者都有真实实现(snapshot 是独立表、clone 是同类型 +
     源指针),而那两处实现细节反过来成了 D6.1a 最有力的两条论据(快照该不该是独立类型、`parent` 该不该
     带 FK),见该节。
   - 一个**值得记下的形状**(若将来要委托直连访问):给每个卷发一个独立作用域的 token,作为其内容数据面
     唯一接受的凭证,且列表接口**故意不返回它**——这把"可以枚举卷"与"可以读写这个卷的字节"干净分开。
     那个实现缺轮转/过期(token 在卷的整个生命期内静态、不销毁卷无法吊销),**缺的这半不值得一起抄**。
3. **第三条方向相反:直连内容访问的**能力**上两者与我们一致,机制上不一致。**(源码已核对。)
   两者都提供**不需要任何沙箱**的宿主侧卷内容读写:一个是 orchestrator 上 9 个 RPC 的独立服务(create/delete
   + dir/file/path 两类,文件走 1 MiB 分块**流式**双向,请求只带 `{volume_id, volume_type, team_id}` + 路径,
   签名里根本没有沙箱);另一个把同一套能力做成 HTTP 文件 API,并在代码里明说它**必须**能在没有活沙箱时工作
   ——它的卷记录带一对默认 POSIX uid/gid 字段,**存在的理由就是"没有活沙箱可以提供身份"**。这条汇合不是巧合:
   一个生命周期独立于沙箱的对象,内容进出却依赖沙箱,那它的独立性只在记录层面(D5.1)。
   - **但机制上我们不抄。** 其中一个为路径限定付的是**每请求一个新 mount namespace + bind + `pivot_root`**
     (还要 root、要 `LockOSThread` 的线程所有权协议),同机实测比 `openat2(RESOLVE_IN_ROOT)` 贵 **342×**
     (两个数字见 D5.1),且要特权。它们的形状不是"安全需要这么贵",是那份代码早于(或忽略了)
     `RESOLVE_IN_ROOT`。**抄能力,不抄机制。**
   - **两者的另一半我们明确不抄:它们都没有在这条路径上放任何与挂载的协调。** 一个的宿主侧写与 guest 的活
     NFS 挂载各自独立打开同一棵树、无锁无租约无版本戳(两处调用点都已核对);另一个的删除是"删行 + 返 204 +
     一个游离 goroutine 去删字节,注释写着『失败了以后再清』",而**那个『以后』的清理程序在整个仓库里不存在**
     (已按 cron / 周期任务 / sweeper 逐类搜过)。我们在同一处放**写入 regime 不变量 + in-flight 标记**
     (D5)与**两个 sweep 面**(D7)。这是同一个能力上两种完全不同的完成度,**差别正是引擎该守的那部分**。
