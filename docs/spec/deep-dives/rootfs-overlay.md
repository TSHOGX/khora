# Rootfs / Overlay 载体:持久层怎么真正落地

> **定位:[`§5.B`](../subsystems.md)(spec,导航见 [`SPEC.md`](../SPEC.md))与 [`§3.1.1`](../model.md)、
> [`§4.2`](../model.md) 的持久层深挖文档。** 载体经真实 substrate 实测定案,结论在 spec §2.5 / §5.B;
> 本文保留结论背后的**证伪过程、实测证据、被排除的备选、要守住的 seam,以及仍待细化的议程**(D5)。
>
> **验证依据:** 在真实 runsc + 内核 overlayfs 上端到端实测(D0 的三条属性、D1 的三条假设逐条对照)。

---

## D0. 一句话结论

**持久层不用 runsc 自管的 overlay**(`--overlay2` 全族 `memory`/`self`/`dir=` 均不跨容器生命周期
持久——实测 `dir=` 启动即 `unlink` filestore,干净停 = 数据蒸发,D1)。**载体改为 engine 在 runsc
外自管的宿主内核 overlayfs**(物化方式见 D3)——此时 upper 是宿主上真正的普通目录树:tar 得动、
可 diff、冷启挂得回、字节一致,兑现 §5.B 的全部附加能力。**持久单元 = 留驻 upper 目录**
(content-addressed diff 层是演进项,定案与代价见 D3)。

这条结论**不动模型层**:§3.1.1「Overlay 是 ID 寻址、与宿主路径解耦的冻结层」、§4.2「pause 零
捕获、resume 冷启重挂」原样成立。runsc 的 overlay flag 不提供跨容器生命周期持久的 upper
(§2.5 事实核查),那个 upper 必须由 engine 在 runsc 外自管。

**实测的三条属性**:CoW 正确(改 lower 只落 upper,lower 字节未污染)、干净停后
upper 是宿主普通目录树且 tar 出完整 diff 层、容器内 sha256 与停机后宿主直读逐字节一致;冷启重挂
同一 upper、以及从 tar 出的 diff 解到全新 upper 重建,内容均逐字节一致(后者顺带证明 diff 层可
提取/搬运/重建,即 D3 演进项的可行性证据)。

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

## D2. 载体形状与演进目标:外层 overlayfs + content-addressed diff

外层 overlayfs 载体(①②,物化见 D3)天然容纳一条成熟的演进路径(③),它决定了 §3.1.1 的抽象形状。
三步的持久化**完全不碰 runsc 的 overlay**:

**① runsc overlay 关掉**(`--overlay2=none --file-access=shared`):runsc 拿到的 rootfs 是**引擎在
外面已经挂好的宿主内核 overlayfs 的 merged 目录**,经 gofer(`file-access=shared`)代理进 guest。

**② upper 是内核 overlayfs 的普通目录树**:merged mount 的 `upperdir=` 就是宿主上一个真实路径,可
直接 `filepath.Walk` 遍历——与 gVisor 那个启动即 `unlink` 的 filestore 是两个世界(D1)。

**③ 这个载体容得下一条 content-addressed diff 的演进路径**(持久单元本身是**留驻 upper**,已定,见 D3;
③ 说的不是"持久单元其实是别的东西",而是"同一个抽象之下**还能**多长出这条路径"):
- 把 upper 提取成 OCI diff tar + `{MediaType, Digest, Size}` descriptor,推到 object store。
- resume/claim 时把 diff apply 回**新容器**的 live rootfs bind mount 上。
- 相对某 baseline 层做**增量** diff——天然分层。

**关键洞察**:持久单元**不必**是「一个一直躺在盘上的 upper 目录」——今天它就是,但抽象不靠这一点成立。
**这正是 §3.1.1 的赌注**——把 Overlay 定义成「ID 寻址、与宿主路径解耦的冻结层」而非「一直在
本地盘的 upper」,于是换持久单元不动模型。这套能力与模型逐条对上(演进项的定案与代价见 D3):

| diff 层能力 | 怎么给 | 对应我们的 § |
| --- | --- | --- |
| pause / resume | 最新 diff = latest checkpoint,resume 时 apply 回新容器 | §4.2 |
| Promote → 分层 Template | 相对 baseline 做增量,层层叠 | §5.B Promote / 分层 Template |
| 跨 host 可移 | diff 是 object store 里的 OCI blob,不锁本地路径 | §5.B「纯文件层可移」+ §3.1.1 |
| fork / clone | 从同一 diff/snapshot claim 多个沙箱 | §3.1 fork |
| snapshot / restore | 命名 diff 记录,可 list/restore/delete | (无对应 §,纯演进期能力) |

---

## D3. 物化方式

**engine 在 runsc 外用宿主内核 overlayfs 拼 lower(Template)/upper/work,merged 交给 `--overlay2=
none --file-access=shared` 的 runsc。** 这守住了三条既定属性:§3.1 三态(Template 只读共享 +
Overlay 增量)、§5.B 底层存储附带能力(CoW 落 reflink)、§3.1.1 载体无关。overlayfs 的 mount/umount 生命
周期归 Rootfs 子系统(§5.B)。

**持久单元 = 留驻 upper 目录。** upper 就地留在后端,pause 零捕获、resume 重挂同一 upper 冷启——
最贴近 §4.2 的语义,兑现 pause/resume 与 Promote 的 at-rest 读已足够。**content-addressed diff 层
(提取成 OCI tar + digest)是演进项**:它换来分层 Template / fork / 跨 host 白送,代价是 tar 提取 +
baseline 增量 + object store 接口。二者共用同一个 Overlay 抽象,上层无感,由 D4 的 seam 担保。

**被排除的备选——bind mount 作可写层**(给容器一个宿主 bind 目录当可写 rootfs,同样 `overlay2=
none`):最简单,但**丢了 CoW 分层**——Template 不再是只读共享底座,每个沙箱得完整拷一份 rootfs;
Promote/fork 要靠外部 CoW FS(reflink/btrfs/zfs)另做。与 §3.1 三态模型不契合;若底层 FS 支持
reflink 来补分层,那其实又滑回上面的 overlayfs 底层存储附带能力,不如直接用 overlayfs。故不采用。

---

## D4. 归属与要守住的 seam

**归属(§4「动词归 §4,机制归 §5」+ 子系统不互调)**:overlayfs 的 mount/umount 是 **§5.B Rootfs
子系统**的机制,由 §4 生命周期在 create/resume 时**调用**——不是让 §5.A Runtime 去拼 overlay,
也不是 Rootfs 去戳 runtime provider。runsc 只收一个**已经 merged 好的 rootfs 路径**作为
`(rootfs 来源)`,这正好落在 §5.A provider 契约的 `(rootfs 来源, …) → (placement 句柄, …)`
入参里——**provider 不关心 rootfs 怎么拼出来的**,拼装是 Rootfs 子系统的事。

**要守住的 seam(留驻 upper 不得堵死 diff 层演进和未来 provider)**:

1. **Overlay 抽象对上层只暴露「ID + 引用的 Template + at-rest 可读」,不暴露「它是一个宿主
   upper 路径」**——这样从留驻 upper 演进到 content-addressed diff,§4.2 的 resume、§3.1 的
   Promote/fork 代码**一行不改**。宿主路径必须锁死在 Rootfs 子系统内部(§3.1.1「对内也 ID 寻址」)。
2. **物化经 rootfs provider seam**(§5.B):唯一实现是 single-node 的「本机内核 overlayfs +
   本地目录」;diff 层(演进项,见 D3)/ multi-node(PV / 对象存储)是**注册另一个 rootfs provider 或换
   持久单元**,不改模型——抽象须容得下它。
3. **不引入 containerd**(与 §1.1「单二进制、无重型外部依赖」冲突):复用 containerd overlayfs
   snapshotter 只在已经跑在 containerd 上时才划算。我们**在引擎内薄薄实现**「组装 overlayfs
   +(演进期)提取 diff」这一层,不拉整个 containerd 栈。这层薄实现本身就是上面 seam 2 的
   single-node provider。

---

## D5. 这条载体自带的机制约束(附一条演进项)

> 载体与持久单元(留驻 upper)之上,本节记这条载体自带的约束。**除最后一条(content-addressed diff)以外
> 都已定**——已否决的按已否决记,不留成待办。

- **work dir 与 upper 的清理/回收**:overlayfs 需要一个 workdir(约束:与 upper 同一个支持 overlayfs
  的 FS)。**resume 重挂用同一 upper,但 workdir 必须新建一个空目录**——不可复用上次的 work(内核会
  拒/坏)。分两半挂:**destroy 时的清理**挂 §4.3 reaper 的对象回收不变量(upper 是留驻持久单元);
  **崩溃后的孤儿回收**——overlay mount 是 placement-local 资源,orchestrator 崩在转换中途留下的半建
  mount 是 §4.4-A 崩溃变体(重启对账)的一个**特例**,不靠 refcount GC。overlay mount 由 engine 在
  runsc 外建立/卸载,卸载时序与 §4.2 pause 对齐。
- **mount 是特权操作**:overlayfs mount/umount 须 root(CAP_SYS_ADMIN),copy-up 落进 upper 的文件因此
  **root-owned**——destroy 删 upper 也须特权。这正是「宿主路径 + mount 生命周期锁死在 §5.B 子系统内、
  不外露给调用方」(§3.1.1)的又一条落地理由:特权 mount 是子系统机制,不是 mediator 的活。
- **容量配额(disk 容量轴,§3.4)**:per-sandbox 的 upper+work 容量帽怎么物化,见 D6。
- **pause flush 一致点**:**substrate 半已定并被依赖**——干净停(kill+delete)后 sentry/gofer 的写
  flush 到 upper 的 at-rest 一致性已验(D0/D3),Promote 读的正是这个抢占式停后的 at-rest upper
  (guest-runtime D4.4:不依赖进程配合)。**应用层协作安全点不是开放项,是已否决项**(guest-runtime
  D4.4:engine 不认识 agent,既不能要求负载停到安全点、也无法验证它停对了;超时/强杀语义与崩溃一致,
  而崩溃是 §4.2 已接受的底线),故这条载体上**没有仍开放的一致点问题**。
- **excluded paths / portal paths**:提取 Promote/snapshot 时要过滤掉引擎自身注入的运行时路径
  (如 guest 内 daemon 的工作目录)。
  - **sandboxd 的两个 portal 文件(transport pidfile / crash-loop sentinel)是清单里的惰性项**:`/tmp` 由
    OCI spec 挂成 **tmpfs**,故它们**根本不进 upper**——排除它们针对的是一个物化上不可达的状态(其守卫
    测试因此在 assert 一个基底造不出来的局面,读起来像覆盖、实际为空)。留着无害(一条 list 项比一个
    "每个沙箱生来不能服务的 Template"便宜),但**它们不是这条机制存在的理由**。
  - **真正落进 upper 的引擎注入物是大文件旁路的上传临时文件**(按 base-name 前缀排除,因为确切路径事前
    不可知——它落在调用方自己指定的目录里)。这才是排除机制的活消费者。
  - **空占位不需要排除项。** 实测:dest 在 lower 与 upper 都不存在时,`runsc create` 之后(任何 guest 代码
    之前)upper 里就已出现一个**普通空目录、mode 0755 root:root、零 overlayfs xattr**——不是 whiteout、
    不是 opaque、无 origin/redirect,且不继承源目录的权限。故**是"放置"物化了它,不是 payload**。
    §5.B 已写的结果("挂载点在固化结果里只是一个空目录占位")默认就成立。反向 hazard 也干净:guest 在
    bind 下的写**全部**落到 Volume 宿主目录、upper 一无所得(有正向对照:同一次 launch 里另一个控制文件
    确实进了 upper),即 **Volume 写入正确地落在沙箱 disk 帽之外、也在 Promote 捕获之外**。
  - **尖锐的不对称:dest 缺失会脏 upper,Template 提供 dest 则 upper 纯净。** lower 里已有的 dest **不触发
    copy-up**(实测后 upper 逐字节为空)。**设计副产品**:在内置 Template 里预置常用挂载点能让常见路径保持
    干净,排除项则退化为正确性 backstop 而非常规机制。对用户自选 dest 不可依赖。
  - **排除项真正要干的是剪掉被挂载点遮蔽的既有字节。** 实测:先在 upper 里种下 `dest/stale`、再用 bind
    遮住它,guest **完全看不到**那些字节(只见 Volume 的条目),而它们仍在盘上、仍在 upper 里——于是会被
    完整拷进冻结层,**并同时计进该沙箱的容量帽与代价预报**。既有机制已能正确做到剪除(排除项匹配到目录即
    跳过整棵子树),**缺的不是匹配逻辑**。
  - **因此挂载点集合必须是 per-Promote 入参,不能是静态包级清单**:Volume dest 是 per-sandbox 的挂载行,
    静态清单无从得知。作为参数传入也是架构上更干净的路——§4 mediator 本来就持挂载表、本来就调 Promote,
    故 **§5.B 永远不必去问 §5.C**(不产生子系统互调)。**代价预报走同一个集合**,否则它会继续把不会被拷的
    遮蔽字节算进去(方向安全,但已知是错的)。路径按既有约定:**upper 相对、去掉前导斜杠**。
  - **嵌套 dest 需要一个显式决定,已定**:嵌套 dest 的**整条链**都会进 upper(三级皆不存在时三级都被创建),
    故只排叶子会在层里留下幽灵中间目录,而排顶层组件在一般情形下是错的(guest 可能合法地写了该目录下的
    别的东西)。**定为:剪叶子子树、接受空的中间链**——它与 §5.B 已认可的空占位同形、只是多一级,
    且**永不丢真实 guest 数据**。
- **content-addressed diff 层**:这条载体上**唯一真正仍开放**的一项。定案与代价见 D3,能力映射见 D2。

---

## D6. 容量配额:per-sandbox upper 帽怎么物化

> **定位:[`§3.4`](../model.md) disk 容量轴 + [`§5.B`](../subsystems.md) 容量配额职责的深挖。**
> 载体(外层内核 overlayfs,D0)决定了 upper 是宿主普通目录树,这恰好让最轻的配额手段可用。

### D6.1 问题:内核 overlayfs 无内置尺寸上限

外层内核 overlayfs(D0)只解决"持久 + 可移 + 可 diff",**不带容量上限**:upper 是普通目录树,能写
多少取决于它所在 FS 的剩余空间。缺配额时,单个沙箱 `dd if=/dev/zero of=big` 就能吃光 upper 所在盘,
让**同盘所有沙箱**的 upper 写失败——击穿 §2.3「放大只发生在本沙箱数据面内」。容量上限因此必须由
rootfs provider 显式施加(§5.B),这与 D0 载体正交:载体给"普通目录树",配额给"这棵树最多多大"。

### D6.2 物化手段(single-node):btrfs subvolume + squota 为首选

| 手段 | 隔离强度 | 代价 | 适配 |
| --- | --- | --- | --- |
| **btrfs subvolume + squota limit**(给 upper+work 所在 subvolume 打一个配额硬限) | 中,硬 `ENOSPC` 到帽 | 最轻:配额在本 substrate 已 fs-wide 启用,两条 `btrfs` 子命令,无额外挂载;subvolume 同时兑现廉价原子快照 | **single-node 首选** |
| **loop 设备**(每沙箱一个定长 image 文件,内建 FS 后挂载,upper/work 落其中) | 强,物理定长隔离 | per-sandbox loop dev + mount 生命周期管理,资源更重 | 需要强物理隔离时 |
| **独立 FS / thin LV**(每沙箱一个 thin volume 或独立分区) | 最强 | 最重,依赖 LVM/卷管理 | 特殊高隔离场景 |

三者是**同一个存储 substrate 原语的不同实现分支**,抽象对上层一字不变:上层只见"这个 Overlay 有个容量帽
(Binding)",不见帽是 squota 还是 loop image。

> **首选 = btrfs subvolume + squota(实测,全栈)。** substrate(ADR-0002)是 **btrfs**,它**无 project
> quota**,故 project-quota 类手段(xfs/ext4)在本盘不适用。实测穿透
> `subvolume → overlayfs upper/work → gofer → sentry → guest` 整条链:写满触发**精确到帽的硬 `ENOSPC`**
> (errno 28 在 `write()` 上报,`fsync`/`close` 返 0,落点紧贴帽——一次性实测 ~0.05%,**但这个数字只能引作
> 量级证据、不可引作可复现的精度指标**:没有脚本打印它,而唯一断言 ENOSPC 精度的
> `spike-volume-cap-snapshot.sh` 用的容差远宽于它,8 MiB 帽上 ±2 MiB),**容器照常存活**(可再 exec),
> 稀疏文件 / `fallocate` / hardlink / 从 lower rename 均未找到绕过账目的路径。选它还有一个结构收益——把
> 存储单元定为 subvolume,同时给 Volume(§5.C)兑现廉价原子快照(volume.md D6/D7)。

**为何是 squota 而非完整 qgroup。** 三条:① squota 就是为"每工作负载一个 subvolume"设计的
(低开销、无 rescan 风暴);② **语义才对得上要管的东西**——沙箱**新写的**字节。完整 qgroup 下 copy-up 的
reflink 会把 `referenced` 顶过帽(实测 98 MiB / 64 MiB 帽),事后才靠拒新写回落,**付了代价仍拿不到硬顶**;
而向一个沙箱收取它从 Template reflink 来的字节(内容不是它写的)本身就存疑;③ 开发/测试基底(`sbx`)
**改不了模式**(`BTRFS_IOC_QUOTA_CTL` 即便 root 带全 `CAP_SYS_ADMIN` 也 EPERM),选完整 qgroup 等于让
spec 与基底长期分叉(CLAUDE.md 警告的漂移)。

> **代价,必须连带处理**:squota 下沙箱撑不破自己的帽,但 **Template / layer 字节仍占真实磁盘且无帽
> 覆盖**。**这就是节点核算式第二项存在的理由**——只算帽的总和会漏掉这些字节,式子本身在 §3.4。
> **注意第二项的判据是"无帽覆盖",不是"是不是 layer"**:有帽的 Overlay upper 已被第一项计过,不重复计;
> 无帽的字节(layer,以及被允许存在时的无帽 upper)才进第二项。

**copy-up 撞帽不会损坏 merged 视图(实测)。** 跨 fs 强制真拷贝时,`open(O_WRONLY)` 干净地返 errno 28,
文件在 merged 里**尺寸正确、与 lower 原件逐字节一致**,upper/work 无残留半成品——overlayfs 拷进 workdir
临时文件、成功才 link 进 upper,失败完整回滚。**但同 fs 上 copy-up 是 reflink**,不受帽约束地被接纳,
故帽对 copy-up 不是硬顶(见上,squota 下 reflink 永久免费,这恰是选它的理由之一)。

**`btrfs-progs` 是显式依赖**(`sbx` 上已装,实测 v6.17.1)。另一条实现时会撞的坑:**sentry 伪造
`st_blocks`**——guest 对 512 MiB 稀疏文件报 `536870912`,宿主真实值为 `0`。**容量核算只信配额,永不信
guest 侧数字**(§5.B)。

### D6.3 关键约束:work 必须与 upper 同配额(subvolume 天然满足)

overlayfs 的 copy-up(改 lower 文件时把它整份拷到可写层)**先落 workdir、再 rename 进 upper**。
若只给 upper 打配额、漏掉 work,copy-up 期间的写就是一条**绕过帽**的路径。D5 的实测约束"work 与
upper 必须同一个支持 overlayfs 的 FS"在这里进一步收紧为:**work 与 upper 必须记进同一个配额单元 /
同一个 loop image / 同一个 thin volume**——容量核算把两者当一体。**btrfs subvolume 天然满足**:upper 与
work 同处一个 subvolume → 一个配额覆盖两者,无绕过路径(实测:`work`/`upper` 在 merged 视图里不可见,
copy-up 流量全计入)。

### D6.4 归属:一个跨 §5.B/§5.C 共享的 substrate 原语,不产生子系统互调

容量帽的物化机制(create-subvolume + set-quota-limit + delete-subvolume)对 **§5.B 的 Overlay upper+work
子树**与 **§5.C 的 per-volume 单元**是**同一套**——它是一个**共享存储 substrate 原语**,跨两个子系统复用。

> **共享的是代码,不是 seam。** 它落成一个**叶子包**(`internal/btrfs`),只包 subvolume
> create/delete/snapshot + quota limit,**不认识 Overlay / Volume / SandboxID**;§5.B 与 §5.C 各自
> import 它,如同各自 import `os`。**不做一个 `QuotaProvider` 横切接口**——远端 rootfs provider 会用完全
> 不同的手段兑现容量帽(对象存储配额 / 配额 API),横切接口只会把 btrfs 假设焊进所有实现。两者都不调
> **对方**,只是共用一个 substrate 工具,故不违反"子系统间不互调"。**Overlay upper 是首个消费者**
> (§5.B),Volume 复用同一个包。

施加动作全程在存储物化侧完成,不调用别的子系统;帽的
**取值**(Binding)由 §4 mediator 在 create/resume 编排时连同 rootfs 来源 / 挂载表一起下发——这本就是
mediator 的职责,不违反"子系统间不互调"(§4 开头)。multi-node 形态下,PV-backed 单元的容量帽 = PV 的
size request,仍是那一侧后端的活,抽象不变(§3.1.1)。

### D6.5 边界:帽之外还有两层行为

D6 只解决**per-sandbox 帽怎么物化**——把"撑满整机"降级成"撑满自己"。另两层:

- **打满时的标准行为**:guest 见普通 `ENOSPC`、**沙箱照常存活**(§3.4:容量超限是数据面内的事件,
  控制面不介入)。**只做硬阈拒写**——不发容量压力事件、不触发 auto-pause。**软阈已定:不发事件、
  不新增 event kind**(§4.4-C)——压力是连续量的采样,§4.1 事件流是离散状态转换的流;要监控在引擎外
  按 gauge/采样接(§1.3 add a subscriber)。
- **节点级不超卖**:不等式在 §3.4,第二项为何必须存在见 D6.2。此处只定**谁在何时核算**:create / resume
  物化 upper 前由 §4 mediator 过一遍;不足则 create 失败 / resume 留 `paused`(§4.4-A 原子退回源态)。
  **有限超卖 + 磁盘压力监控不在此形态内**(后续增量)。

  两项**互斥**的判据("对每个对象取最强的可得陈述"、有帽的 Overlay upper 在实测项里计 0)归
  §3.4;本文只补它成立的机制根据:squota 下 reflink 的 copy-up 不向沙箱单元计费、该 extent 在 layer
  一侧计过——**迫使第二项存在的性质(D6.2),恰好也让两项天然不重叠**。该轴被计量后每个沙箱必须有帽
  (否则不等式不再描述那块盘)的推理同在 §3.4。

  **残留(诚实说)**:第二项是实测,故不等式在**准入那一刻**成立、之后可被破坏。layer 字节只在
  Promote / seed / sweep 时增长——都是控制面事件,**故可门控**;而 §4.3 的对象回收 sweep 会让它减少。
  任何增加无帽字节的控制面动作都过同一道不等式。**Promote 是其中唯一已有动词的一个**,seed / sweep
  待其动词出现时同办;它的三个判据:

  - **门在 quiesce 之前**:`Promote` 先只开 Overlay 取句柄(不动帽)、问 provider 要一个代价预报,不足即拒
    (§6 映 507)。此时**源沙箱仍在它的入口态**、不曾被冻结,故一次被拒的 Promote 不是一次可观测的转换
    (§4.4-A 原子退回的同一条纪律)。
  - **这一项是预报,不是实测**:代价由 `rootfs.Provider.PromoteCostBytes` 给出,契约是**上界**——它必须在
    动作之前回答,而第二项别处都是实测。保守方向是刻意的:高估拒掉一个本可放下的 Promote,低估则把
    ENOSPC 摊给无关沙箱(§2.3)。一个 reflink / snapshot 实现可以诚实地答约 0。
  - **在飞预留不是第三项**:字节要等拷贝推进才被 `UncappedBytes` 看见,而拷贝太长、不能全程持节点级
    锁,故 mediator 在放锁前登记一笔预留,充当 Promote 的"提交"(等价于 create 那一刻的插行),落地后
    释放。它与第二项**同项**,只是让并发的门看见在飞的那部分;拷贝落地期间短暂**双算**,方向同样保守。
