# Rootfs / Overlay 载体:持久层怎么真正落地

> **定位:[`§5.B`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))与 [`§3.1.1`](../model.md)、
> [`§4.2`](../model.md) 的持久层深挖文档。** 载体经真实 substrate 实测定案,结论在 spec §2.5 / §5.B;
> 本文保留结论背后的**证伪过程、实测证据、被排除的备选、要守住的 seam,以及仍待细化的议程**(D5)。
>
> **验证依据:** 在真实 runsc + 内核 overlayfs 上端到端实测(下述三条要求逐条对照)。

---

## D0. 一句话结论

**持久层不用 runsc 自管的 overlay**(`--overlay2` 全族 `memory`/`self`/`dir=` 均不跨容器生命周期
持久——实测 `dir=` 启动即 `unlink` filestore,干净停 = 数据蒸发,D1)。**载体改为 engine 在 runsc
外自管的宿主内核 overlayfs**(物化方式见 D3)——此时 upper 是宿主上真正的普通目录树:tar 得动、
可 diff、冷启挂得回、字节一致,兑现 §5.B 的全部附加能力。**持久单元先做「留驻 upper 目录」,
content-addressed diff 层是后续演进项(详见 D3)。**

这条结论**不动模型层**:§3.1.1「Overlay 是 ID 寻址、与宿主路径解耦的冻结层」、§4.2「pause 零
捕获、resume 冷启重挂」原样成立。runsc 的 overlay flag 不提供跨容器生命周期持久的 upper
(§2.5 事实核查),那个 upper 必须由 engine 在 runsc 外自管。

**端到端实测,三条要求全通过**:CoW 正确(改 lower 只落 upper,lower 字节未污染)、干净停后
upper 是宿主普通目录树且 tar 出完整 diff 层、容器内 sha256 与停机后宿主直读逐字节一致;冷启重挂
同一 upper、以及从 tar 出的 diff 解到全新 upper 重建,内容均逐字节一致(后者顺带证明 diff 层可
提取/搬运/重建,是 D3 演进项的实测底气)。

---

## D1. 为什么不用 runsc 自管 overlay(证伪过程)

一个看似可行的路径是直接押 runsc `--overlay2` 的 `self` / `dir=/path`:假设它把可写层落成宿主上
的普通目录树,`dir=` 的 upper 冷启能重新挂回且字节一致。**这条假设实测全不成立**,逐条对照:

| 假设 | 实测结果 |
| --- | --- |
| upper 是宿主普通目录树 | 是单个 `runsc-filestore-*` 文件,且**启动瞬间就被 `unlink`** |
| 干净停掉后能冷启重新挂回 | 数据随进程退出蒸发,同一 upper 冷启是空的 |
| 字节一致 | 文件根本不在了,无从谈起 |

**机制**:gVisor 的 `--overlay2` 不是内核 overlayfs,是 sentry 自有的 overlay 格式。`dir=` 的语义
是「filestore **落在这个目录所在的磁盘上**」(用磁盘而非 RAM 做 overlay 后备存储,控制大小),
**不是**「把 upper 以普通目录树持久化到宿主」。flag 文档原文是 "filestore will be created",
一开就 `unlink`,只靠 sentry 的 open FD 撑着,最后一个 FD 随进程关闭即回收。

这也一并排除了「sentry-owned overlay」这条看似的退路——它**就是** `--overlay2` 这一族,同一个机制,
不是另一条路。所以要换范式(D0:engine 在 runsc 外自管 overlayfs),而不是在 runsc 自管 overlay 里
换档位。

---

## D2. 演进目标:外层 overlayfs + content-addressed diff

留驻 upper 之上有一条成熟的演进路径,值得现在就想清楚,因为它决定了 §3.1.1 的抽象形状。它的持久化
**完全不碰 runsc 的 overlay**,三步:

**① runsc overlay 关掉**(`--overlay2=none --file-access=shared`):runsc 拿到的 rootfs 是**引擎在
外面已经挂好的宿主内核 overlayfs 的 merged 目录**,经 gofer(`file-access=shared`)代理进 guest。

**② upper 是内核 overlayfs 的普通目录树**:merged mount 的 `upperdir=` 就是宿主上一个真实路径,可
直接 `filepath.Walk` 遍历——与 gVisor 那个启动即 `unlink` 的 filestore 是两个世界(D1)。

**③ 持久单元是 content-addressed diff,不是留驻的 upper**:
- 把 upper 提取成 OCI diff tar + `{MediaType, Digest, Size}` descriptor,推到 object store。
- resume/claim 时把 diff apply 回**新容器**的 live rootfs bind mount 上。
- 相对某 baseline 层做**增量** diff——天然分层。

**关键洞察**:持久单元不必是「一个一直躺在盘上的 upper 目录」,可以是「一个内容寻址的 diff 层」。
**这正是我们 §3.1.1 的赌注**——把 Overlay 定义成「ID 寻址、与宿主路径解耦的冻结层」而非「一直在
本地盘的 upper」。这套能力和我们模型逐条对上,也正是我们把 diff 层留作演进项(D3 / D4 seam)的原因:

| diff 层能力 | 怎么给 | 对应我们的 § |
| --- | --- | --- |
| pause / resume | 最新 diff = latest checkpoint,resume 时 apply 回新容器 | §4.2 |
| Promote → 分层 Template | 相对 baseline 做增量,层层叠 | §5.B Promote / 分层 Template |
| 跨 host 可移 | diff 是 object store 里的 OCI blob,不锁本地路径 | §5.B「纯文件层可移」+ §3.1.1 |
| fork / clone | 从同一 diff/snapshot claim 多个沙箱 | §3.1 fork |
| snapshot / restore | 命名 diff 记录,可 list/restore/delete | Promote 的超集 |

---

## D3. 物化方式(已定)

**engine 在 runsc 外用宿主内核 overlayfs 拼 lower(Template)/upper/work,merged 交给 `--overlay2=
none --file-access=shared` 的 runsc。** 这守住了三条既定属性:§3.1 三态(Template 只读共享 +
Overlay 增量)、§5.B 底层存储附带能力(CoW 落 reflink)、§3.1.1 载体无关。overlayfs 的 mount/umount 生命
周期归 FS 子系统(§4)。

**持久单元:先做「留驻 upper 目录」。** upper 就地留在后端,pause 零捕获、resume 重挂同一 upper
冷启——最贴近 §4.2 的原文语义,上手最快,兑现 pause/resume 与 Promote 的 at-rest 读已足够。
**content-addressed diff 层(提取成 OCI tar + digest)是后续演进项**,不是现在要做的:它换来
分层 Template / fork / 跨 host 白送(参考实现走这条),但要额外实现 tar 提取 + baseline 增量 +
object store 接口。二者共用同一个 Overlay 抽象,上层无感——先做前者、后者随 roadmap 对 fork/
multi-node 的优先级演进,靠 D4 的 seam 保证演进时上层不改。

**被排除的备选——bind mount 作可写层**(给容器一个宿主 bind 目录当可写 rootfs,同样 `overlay2=
none`):最简单,但**丢了 CoW 分层**——Template 不再是只读共享底座,每个沙箱得完整拷一份 rootfs;
Promote/fork 要靠外部 CoW FS(reflink/btrfs/zfs)另做。与 §3.1 三态模型不契合;若底层 FS 支持
reflink 来补分层,那其实又滑回上面的 overlayfs 底层存储附带能力,不如直接用 overlayfs。故不采用。

---

## D4. 归属与要守住的 seam

**归属(§4「动词归 §4,机制归 §5」+ 子系统不互调)**:overlayfs 的 mount/umount 是 **§5.B FS
子系统**的机制,由 §4 生命周期在 create/resume 时**调用**——不是让 §5.A Sandbox 去拼 overlay,
也不是 FS 去戳 runtime provider。runsc 只收一个**已经 merged 好的 rootfs 路径**作为
`(rootfs 来源)`,这正好落在 §5.A provider 契约的 `(rootfs 来源, …) → (placement 句柄, …)`
入参里——**provider 不关心 rootfs 怎么拼出来的**,拼装是 FS 子系统的事。

**要守住的 seam(现在做留驻 upper,但别堵死 diff 层演进和未来 provider)**:

1. **Overlay 抽象对上层只暴露「ID + 引用的 Template + at-rest 可读」,不暴露「它是一个宿主
   upper 路径」**——这样从留驻 upper 演进到 content-addressed diff,§4.2 的 resume、§3.1 的
   Promote/fork 代码**一行不改**。宿主路径必须锁死在 FS 子系统内部(§3.1.1「对内也 ID 寻址」)。
2. **物化经 FS provider seam**(§5.B):今天唯一实现是 single-node 的「本机内核 overlayfs +
   本地目录」;diff 层 / multi-node(PV / 对象存储)是**注册另一个 FS provider 或换持久单元**,
   不改模型。参考实现把 diff 推 object store 正是这个 seam 的一种实现——按 D3,现在做留驻 upper,
   抽象要能容纳后续的 diff 层演进。
3. **不引入 containerd**(与 §1.1「单二进制、无重型外部依赖」冲突):复用 containerd overlayfs
   snapshotter 只在已经跑在 containerd 上时才划算。我们**在引擎内薄薄实现**「组装 overlayfs
   +(演进期)提取 diff」这一层,不拉整个 containerd 栈。这层薄实现本身就是上面 seam 2 的
   single-node provider。

---

## D5. 仍待设计的议程(下一轮 / 落地前)

> 载体与持久单元(留驻 upper)已定;下面是**在这条载体上仍要设计的机制**。

- **work dir 与 upper 的清理/回收**:overlayfs 需要一个 workdir(约束:与 upper 同一个支持 overlayfs
  的 FS)。**resume 重挂用同一 upper,但 workdir 必须新建一个空目录**——不可复用上次的 work(内核会
  拒/坏)。分两半挂:**destroy 时的清理**挂 §4.3 reaper 的对象回收不变量(upper 是留驻持久单元);
  **崩溃后的孤儿回收**——overlay mount 是 placement-local 资源,orchestrator 崩在转换中途留下的半建
  mount 是 §4.4-A 崩溃变体(重启对账)的一个**特例**,不靠 refcount GC。overlay mount 由 engine 在
  runsc 外建立/卸载,卸载时序要与 §4.2 pause 对齐。
- **mount 是特权操作**:overlayfs mount/umount 须 root(CAP_SYS_ADMIN),copy-up 落进 upper 的文件因此
  **root-owned**——destroy 删 upper 也须特权。这正是「宿主路径 + mount 生命周期锁死在 §5.B 子系统内、
  不外露给调用方」(§3.1.1)的又一条落地理由:特权 mount 是子系统机制,不是 mediator 的活。
- **容量配额(disk 容量轴,§3.4)**:per-sandbox 的 upper+work 容量帽怎么物化,见 D6。
- **pause flush 到一致点的时序**:字节一致性在"干净停(kill+delete)后"已验;仍需锁定的是 pause
  这个动作把 sentry/gofer 的写 flush 到 upper 的**精确一致点**(§4.2 静止点),确保 Promote/
  snapshot 读到的是完整状态而非半写。
- **excluded paths / portal paths**:提取 Promote/snapshot 时要过滤掉引擎自身注入的运行时路径
  (如 guest 内 daemon 的工作目录)与 Volume 挂载点占位(§5.B「Promote 捕获只含 Overlay 树、Volume
  挂载点是空占位」)。精确清单待定。
- **content-addressed diff 层(演进项,演进条件见 D3)**:落地时要实现 tar 提取 + baseline 增量 +
  object store 接口(能力映射见 D2)。

---

## D6. 容量配额:per-sandbox upper 帽怎么物化

> **定位:[`§3.4`](../model.md) disk 容量轴 + [`§5.B`](../subsystems.md) 容量配额职责的深挖。**
> 载体(外层内核 overlayfs,D0)决定了 upper 是宿主普通目录树,这恰好让最轻的配额手段可用。

### D6.1 问题:内核 overlayfs 无内置尺寸上限

外层内核 overlayfs(D0)只解决"持久 + 可移 + 可 diff",**不带容量上限**:upper 是普通目录树,能写
多少取决于它所在 FS 的剩余空间。缺配额时,单个沙箱 `dd if=/dev/zero of=big` 就能吃光 upper 所在盘,
让**同盘所有沙箱**的 upper 写失败——击穿 §2.3「放大只发生在本沙箱数据面内」。容量上限因此必须由
FS provider 显式施加(§5.B),这与 D0 载体正交:载体给"普通目录树",配额给"这棵树最多多大"。

### D6.2 三种物化手段(single-node)

| 手段 | 隔离强度 | 代价 | 适配 |
| --- | --- | --- | --- |
| **project quota**(xfs/ext4:给 upper+work 子树打一个 project id + 硬限) | 中,可软/硬双阈 | 最轻,upper 是普通目录树天然支持,无额外挂载 | **single-node 首选** |
| **loop 设备**(每沙箱一个定长 image 文件,内建 FS 后挂载,upper/work 落其中) | 强,物理定长隔离 | per-sandbox loop dev + mount 生命周期管理,资源更重 | 需要强物理隔离时 |
| **独立 FS / thin LV**(每沙箱一个 thin volume 或独立分区) | 最强 | 最重,依赖 LVM/卷管理 | 特殊高隔离场景 |

三者是**同一个 FS provider seam 的不同实现分支**(D4 seam 2),抽象对上层一字不变:上层只见"这个
Overlay 有个容量帽(Binding)",不见帽是 project id 还是 loop image。**首版做 project quota**——最贴合
D0 的"upper 是普通目录树"这一既得事实,零额外挂载、软硬双阈天然支持,够用。

### D6.3 关键约束:work 必须与 upper 同配额

overlayfs 的 copy-up(改 lower 文件时把它整份拷到可写层)**先落 workdir、再 rename 进 upper**。
若只给 upper 打配额、漏掉 work,copy-up 期间的写就是一条**绕过帽**的路径。D5 的实测约束"work 与
upper 必须同一个支持 overlayfs 的 FS"在这里进一步收紧为:**work 与 upper 必须记进同一个 project id
/ 同一个 loop image / 同一个 thin volume**——容量核算把两者当一体。

### D6.4 归属与不产生子系统互调

配额施加**全程在 §5.B FS 子系统内部完成**(打 project id、设 loop image 尺寸都是 FS 子系统的
mount/物化动作),不调用别的子系统;帽的**取值**(Binding)由 §4 mediator 在 create/resume 编排时
连同 rootfs 来源一起下发——这本就是 mediator 的职责,不违反"子系统间不互调"(§4 开头)。multi-node
形态下,PV-backed overlay 的容量帽 = PV 的 size request,仍是 FS provider 的活,抽象不变(§3.1.1)。

### D6.5 边界:帽之外还有两层行为

D6 只解决**per-sandbox 帽怎么物化**——把"撑满整机"降级成"撑满自己"。帽被打满时的 ENOSPC 标准
行为、以及允许超卖时的节点磁盘压力准入,是跨子系统的行为与策略,尚未定,见
[`open-decisions.md`](../open-decisions.md)。
