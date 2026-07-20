# Sandbox Engine — 子系统(§5 目标定义)

> 属 [SPEC.md](./SPEC.md);全局 §编号跨文件连续,导航见 SPEC.md 顶部「文档地图」。
> 五个子系统平级、只依赖 §3 共用模型,彼此不依赖——故合为一份。

---

## 5. 子系统(目标定义)

五个子系统**平级**,只依赖 §3 的共用模型(沙箱身份 + store),**不互相依赖**(防成环)。
本节定目标与边界;各子系统待细化的内部机制登记在 [`open-decisions.md`](./open-decisions.md)。

- **5.A Runtime** — runsc 进程生命周期、guest 身份、三态物化(沙箱"怎么跑")。
- **5.B Rootfs** — Template(只读底层)+ 可写 Overlay 工件;pause/resume 与 Promote 都挂在
  这里(沙箱"是什么")。
- **5.C Volume** — 解耦、ID 寻址、独立生命周期的存储(额外"挂了哪些独立数据")。
- **5.D Network** — 每个沙箱可定义的网络可达性策略。
- **5.E Transport** — 宿主↔沙箱的通道:两个通用 RPC 服务(process / fs)。

> Rootfs 与 Volume 分两个子系统,因为它们几乎每根轴都不同:**Overlay 工件**的生命周期**随沙箱**、
> 引用某个 Template、就是 pause/resume 的那份冻结层;Volume 生命周期**独立**、先于/晚于沙箱
> 存活、自带管理 API 与后端。二者**都** ID 寻址、都经 FS provider 物化、只共用宿主 CoW/reflink
> **底层存储**,但模型不共用:Overlay 引用 Template 且随沙箱回收,Volume 谁都不引用、独立回收。

### 5.A Runtime(执行基底)

> 子系统名 **Runtime**,别名"执行基底";与顶层持久单元 **Sandbox**(§3.2)刻意区分——Sandbox
> 是"哪一个沙箱"(持久身份),Runtime 是"用什么、在哪儿把它跑起来"(runtime provider 是本
> 子系统核心 seam)。

- **目标**:让单个沙箱持久、可变、可拿 root、可冷启 from Template。
- 提供 **runsc 进程级 start/stop 机制**(供 §4 的 create/pause/resume/destroy 动词驱动),
  物化三态模型(§3.1)。动词归 §4,本子系统只给机制(§4 开头"动词归 §4,机制归 §5")。
- guest 身份**可配置**:默认非 root + sudo 可提权(§2)。
- **资源上限物化(只管速率轴)**:沙箱的 CPU/RAM/**disk I/O** 上限是 Binding(§3.4),由本子系统在
  每次 placement 时物化——single-node provider 落成 cgroup(CPU/RAM 限额 + `io` controller 的带宽/
  IOPS 节流);别的 provider 落成其调度层的等价资源约束。**disk 容量上限不在此**——它是 FS 层配额,
  归 §5.B(Overlay upper+work)/ §5.C(每个 Volume)的 FS provider(§3.4 两轴切分)。
- **启动**:从 Template 冷启(秒级),可叠加现成 restore / 启动优化。容器 entrypoint 是
  `sandboxd`(init 模式当 PID 1,§5.E.1)——本子系统只负责"把它作为 PID 1 拉起",guest 内的
  init/reaper/re-exec 拓扑归 §5.E.1,不在此展开。
- **runtime provider 是本子系统的核心 seam**:"在哪儿、用什么跑一个隔离的进程上下文" 经一个
  可插拔 provider 接口表达。接口必须用**策略 + 句柄**而非**宿主原语**表达——否则把接口绑死在
  "宿主本地"假设上,将来任何非本地形态的 provider 都无法承接:
  - ❌ 泄漏宿主原语:"给我一个 cgroup 路径 / 一个 netns fd / Overlay 的宿主绝对路径"——一个
    被调度到远端 node 的隔离单元根本给不出这些。
  - ✅ 契约式:`(rootfs 来源, 引流要求, 资源上限, guest 身份) → (placement 句柄, transport
    端点, 生命周期控制 stop/start)`。**引流要求**是 §5.D 给的 ingress 描述符 + default-deny(把
    egress 全量引导进引擎数据面,§5.D),不是完整网络策略——策略 enforcement 与凭证注入在引擎数据面里,
    不在 provider。
  今天唯一实现 `local_runsc` 把引流要求物化成 nft+netns、把 §3.4 的资源上限经 **OCI `linux.resources`**
  下发给 runsc 落成 cgroup(cgroupfs driver)。**cgroup 归属:runsc 从 container-id 自建一个根级 leaf
  cgroup、`delete` 时回收**;引擎**不**自建父层级(单二进制无 systemd scope、也无 kubelet-式 cgroup
  driver 代建父目录——自建父 subtree 即反 §1.1 重造那一层)。故一个**孤儿 CID-cgroup 就是孤儿 placement**
  的可扫信号,§4.4-A 周期对账兜底。provider 是**引流与资源上限的物化方**,不碰策略权威、不进 resolve
  路径(§5.D)。**多 node / 调度 / 跨宿主因此天然位于未来某个 provider 内,核心模型不感知**(§1.1 /
  §1.3:provider 是 registry 的一种实现)。
  - **网络 placement 机制(local_runsc,`--network=sandbox` 实测)**:runsc **只 adopt 一个已存在的
    netns、不自建**——`local_runsc` 先建 netns + veth、**给 guest 端分配 IP**,再经 OCI
    `linux.namespaces[type=network].path` 把 netns 交给 runsc;sentry 加入后在 boot 时把内核侧 L3 地址
    **收进自己的 netstack**(boot 后 `ip netns exec <ns> ip addr` 看不到 global 地址,veth *链路*仍在)。
    推论:**引擎因是自己分配 IP 故本就知道端点**——§3.3/D5.1 冷启链 ⑥「发现 IP」对 local_runsc 退化成
    「读回自己的分配」,不做 boot 后探测(`runsc state` 也不给 IP);且 resume 每次**新建 netns**(§3.3
    换新 IP)天然规避「复用 netns 须重灌 guest 地址」这一实现坑。
- **`local_runsc` 钉死其依赖的整套 runsc flag、不继承默认**——每个键对一条既定保证,`runsc` 的默认值
  跨版本会漂移,而 §5.B 的**跨 runsc 版本可移性**不能建立在"默认不变"上:
  - `--platform=systrap` —— §1.1 no-hypervisor(已是 spec 前提,此处只是把它 pin 死;platform 本身
    仍可插拔,别的 provider 可选 KVM,§1.1)。
  - `--network=sandbox`(userspace netstack)—— `host`/hostinet 让 sentry 直用宿主 socket、syscall
    直透宿主内核,**违反 §2 substrate ①**(故否决,非"另一可选模式");且它是 §5.D 引流(per-sandbox
    netns+nft/TPROXY)与 §5.E transport(netstack-terminated,宿主经 veth IP 拨通已实测)的前提。**代价:
    它拒绝 `--rootless`**,故
    orchestrator/launcher 须 root(或 CAP_NET_ADMIN+CAP_SYS_ADMIN)物化 placement——**sentry 自身仍非特权,
    支柱② 不破**;这是部署 caveat(§1.1「可跑在 CI/容器内」= 该环境须授予这些 cap),不是隔离退让。
  - `--overlay2=none --file-access=shared` —— §5.B/§5.C 持久层不靠 runsc 自管 overlay、gofer 代理宿主
    目录(已钉)。
  - `--systemd-cgroup=false` —— 承上条 cgroupfs driver。
  精确 argv 属 provider 实现细节,不进本节;本节只固定"哪些键承重、各对哪条 §保证"。
- **不含**:FS/rootfs 构成与固化(→ B)、Volume 挂载语义(→ C)、网络策略本身(→ D)、
  宿主↔guest I/O 通道(→ E)。

### 5.B Rootfs(沙箱由什么构成)

- **目标**:让"沙箱是什么"可持久、可冻结、可固化成可复用底座,且持久层**与 placement 解耦**。
- **结构**:rootfs = Template(只读底层)+ 本沙箱可写 **Overlay 工件**(§3.1.1),merged 作为
  可写 rootfs 绑进容器。`apt`/`pip` 与 `$HOME` 全写进 Overlay。
- **一条关键支撑事实,带出全部附加能力**:**不存内存(§4.2)⟹ 持久态就是宿主上的纯文件层**。由此顺带
  得到、无额外成本:
  - **跨 host / 跨 CPU / 跨 runsc 版本可移**:纯文件没有 CPU-feature 锁,随便搬。
  - **可审计 / 可 diff**:静止的纯文件层宿主侧 `tar`/`diff` 即可读。
  - **Promote / 分层 Template**:见下,由 at-rest 可读 enable。
- **只读 at-rest,不读 live**:**只在 pause / stop**(容器已停、runsc 已 flush)读 Overlay 字节,
  **从不**在 guest 运行中读。因此"运行中经 gofer 缓存读到不一致状态"这类 live 一致性问题不在路径上。
  **干净停掉后持久 overlay 是宿主普通目录树、能冷启重新挂回且字节一致**(经 ECS 验证,§2.5);持久层
  **不靠 runsc 自管 overlay**(runsc `--overlay2` 全族不跨容器生命周期持久),而靠下面的外层 overlayfs。
- **载体:外层内核 overlayfs,runsc 不掺和写层**(经 ECS 验证,详见
  [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md)):engine 在 runsc 外用宿主内核
  overlayfs 拼 lower(Template)/upper/work,merged 交给 `--overlay2=none --file-access=shared` 的
  runsc(gofer 代理进 guest)。upper 因此是宿主普通目录树,tar 得动、可 diff、冷启挂得回。
- **Overlay 经 FS provider 寻址,不假设宿主本地路径**:
  - single-node 物化:Overlay 落成宿主 overlayfs 的 upperdir、Template 落成 lowerdir,同机,
    pause 零捕获、resume 重挂同一 upperdir 冷启(§4.2)。持久单元是留驻 upperdir;持久单元的选型
    与演进路径见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D3。
  - multi-node 物化:Overlay 落成可跨 node 寻址的层(PV / 对象存储),resume 在任意 node 挂回
    同一工件。模型层面 Overlay 始终是"引用某 Template 的、ID 寻址的冻结层",宿主路径只是
    provider 实现细节(§3.1.1)。
- **overlay mount 生命周期归 Rootfs 子系统**:merged 的组装/卸载由 engine 在 runsc 外负责(runsc 收
  一个已 merged 好的 rootfs 路径,§5.A provider 的 `rootfs 来源` 入参);upper/work 须同一个支持
  overlayfs 的 FS。这是 §4「机制归 §5」的直接落点——生命周期在 create/resume 时调用它,provider
  不关心 rootfs 怎么拼出来的。
- **Promote**:把当前 Overlay 固化成新 Template 的底层——可写层降为只读底层,Template 因此
  天然分层(像分层镜像)。Promote 需要的仅是"at-rest 读到上层字节去 tar/hash",外层 overlayfs 的
  upper 是宿主普通目录树,直接 tar 即可(§2.5 已验)。
- **分层 Template**:一摞只读层(像 OCI image),Promote 往上叠一层。这是 **FS provider 怎么
  存/引用冻结层**的事,位于 runsc overlay 机制**之上**,与 overlay 由谁管那一层无关。
- **Promote 捕获只含 Overlay 树**:固化沙箱 FS = 固化 Overlay 这棵树。挂载进来的 Volume **不在
  这棵树里**——挂载点(如 `/home/workspace`)在固化结果里只是一个**空目录占位**,Volume 内容不进
  去(它在各自后端,§5.C)。这跟 `tar` 一个挂了 NFS 的目录只 tar 到挂载点空壳是同一回事。
- **底层存储附带能力(single-node)**:snapshot/clone 落在 CoW FS(reflink)上廉价。这是 single-node
  provider 的附加能力,不是模型对宿主路径的依赖。
- **容量配额是本子系统的物化职责(disk 容量轴,§3.4)**:per-sandbox 的 disk 容量上限(Binding)由
  FS provider 在 placement 时施加到 **Overlay upper 子树 + work**(两者同一份配额)。物化手段(project
  quota / loop / 独立 FS)与其权衡见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md)
  D6。**Template 只读、不被沙箱写,不设 per-sandbox 帽**——其增长由 Promote + refcount GC(§4.3)
  控制,只作节点容量核算的输入,是全局核算问题而非隔离问题。
- **不含**:Volume 的生命周期与挂载策略(→ C)、Overlay 跑在哪个进程上下文(→ A)。

### 5.C Volume(解耦的独立存储)

- **目标**:把"额外要独立存活的数据"做成一等公民,生命周期与任何沙箱解耦。
- **定义**:ID 寻址的持久存储单元,先于沙箱建、晚于沙箱亡、未挂载也在;可挂到多个沙箱的
  不同路径,**不进沙箱持久态 / 不进 Promote 捕获**,各自后端。
- **共享/独占策略**:exclusive(挂载租约强制)/ shared-ro / shared-rw(只保证同沙箱内多
  连接(process)的 POSIX 语义;**跨沙箱 rw coherence 引擎不定义(undefined)**——gofer/sentry
  各自缓存让跨隔离边界的共享可变一致性本质不可靠,引擎不承诺、用户文档诚实标出)。
- **单机物化 = gofer 代理宿主目录,与 rootfs 同载体、无 FUSE**:Volume 的单机后端是宿主上一个
  目录(可自带 per-volume 配额),写进 runsc OCI mount spec、由 gofer 在 `--file-access=shared`
  下代理进 guest——与 §5.B 的 merged rootfs 同一条路径。**FUSE 只属远端后端**(multi-node),封在
  那个 FS provider 内、不进核心模型(为何单机不需要 FUSE 见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D1)。
- **未挂载也能操作**:create/delete/list/inspect/snapshot/clone + 直接 import/export
  内容。对外**一律 Volume ID 寻址,不暴露宿主路径**。
- **写入 regime 唯一(横切不变量,防双写撕裂)**:Volume 有挂载与直连 API 两条写入路径,引擎守一条
  不变量——**只要有 live-rw 挂载正持有该卷(某沙箱 `running` 且 rw),直连 API 的变更操作
  (import / restore-into)一律拒绝**;无 live-rw 挂载时直连写才放行。门开在"**活的**写入方",不是
  "在挂载表里":**paused 沙箱不算 live writer**(容器已停、无活 gofer),此时直连写安全,且 resume
  后自然见到新内容。这是控制面读 store 后对直连 API 的准入裁决,
  不引入反向代理(证伪见 [`deep-dives/volume.md`](./deep-dives/volume.md) D3)。
- **快照默认 crash-consistent,与 Overlay 不对称**:Overlay 的 Promote 能强一致,因它**单 owner**、
  可 pause 到 at-rest 读(§5.B);Volume 打破此前提(shared-rw 多 owner、挂载中不想停)。故 Volume
  快照 = 后端(reflink/CoW)对当前盘上字节的原子快照、**不静默写入方**,一致性等价"那一刻拔电源"
  ——正是 §4.2 已接受的"崩溃 = 非自愿 pause"底线。要 app-consistent:**独占卷**由调用方先 pause 其
  owner(复用 §4.2 静止点)再快照,是现有原语的**组合**、引擎不加机制;**shared-rw 不提供**
  app-consistent(协调 N 个沙箱是产品语义,§1.3 否决)。推导见
  [`deep-dives/volume.md`](./deep-dives/volume.md) D2。
- **挂载关系是 Binding(§3.4)**:"挂了哪些、挂到哪、什么策略"记在控制面挂载表,resume 时
  按当前挂载表**重新挂**回去,而非从冻结字节恢复——Volume 永远是那一个真身,不分叉副本。
- **独占租约 = §4.3 的挂载行,不是带心跳的独立机制**:exclusive 卷的"挂载租约"就是 §4.3 那个一等
  持久对象「挂载行」(带生命周期态 + refcount,单机权威 store 里的一行);建 exclusive 挂载行前控制面
  CAS 校验无冲突挂载行,有则拒。**崩溃保持租约是正确行为**:沙箱崩溃 = dead-recoverable(§4.3),它会
  resume 回同一卷,崩溃即释放会让 resume 撞车——与 §4.2「崩溃走 resume 恢复」一致。租约**只在挂载行被
  拆时释放**:沙箱 destroy(含 reaper 对 paused 超 TTL 的 evict)→ 挂载行拆除 → 独占解除,**reaper 的
  TTL 就是"无人释放"的兜底**,不需心跳。心跳式回收是未来 multi-node provider 的义务(node 死亡由其调度
  层接),不进核心(证伪见 [`deep-dives/volume.md`](./deep-dives/volume.md) D4)。
- **挂载拓扑变更发生在 resume 边界,不是热挂**:runsc 从 OCI spec 在**启动时**取挂载,运行中
  **不支持**热加/热卸(仅有 EROFS debug 旁路,不用)。所以"给运行中沙箱挂个新 Volume" = 改挂载表
  Binding → 冷 resume 带新表,与 §4.2 的 FS-first pause/resume 复用同一条路径。
- **挂载拓扑规则**:挂载点宿主目录归 Overlay 工件(沙箱 rootfs);**Volume 之间不嵌套挂载**
  (不把一个 Volume 挂进另一个 Volume 的子树),否则 Promote 捕获里挂载点占位的归属会含糊。
- **容量配额是 per-volume 属性,不是 per-sandbox(disk 容量轴,§3.4)**:一个 Volume 的容量上限
  记在它自己的对象记录上,由 FS provider 在其后端物化;它**不能归给某个沙箱**——shared-rw Volume
  被多个沙箱共写,占用无法拆到单沙箱头上。这与 §5.B 的 Overlay upper 帽(per-sandbox)是两个独立
  的容量帽,各自 enforce。物化手段与 §5.B 同族(project quota / 独立 FS),见
  [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D6。
- **不含**:沙箱自身的 rootfs / Overlay 工件(→ B)。

### 5.D Network(可达性策略)

- **目标**:做"自由"的网络使能者,同时守住安全。每个沙箱**拉起时可定义**网络策略。
- **职责切分(策略 / 数据面 / 引流,三块)**:本子系统**持有策略**(出站 allow-deny、入站 publish
  规则、model 路由 + 凭证路由);策略的**强制与凭证注入由一个引擎自有的 egress 数据面(gateway)承担**
  ——它是本子系统的一个**行为 seam**(可换 builtin / Envoy / 商用网关,与 provider 无关);**引流**
  (把 egress 引导进该数据面)是 **placement-local 资源**,由 §5.A provider 用 nft/netns/TPROXY(或
  CNI/sidecar)物化,与"资源上限是 Binding、cgroup 由 provider 物化"同构。所以 resume 时:**gateway
  由本子系统建/选,引流由 provider 建**;两者经 mediator 编排的两条薄契约(resolve / ingress)相遇、
  不互调(§4;详见 [`deep-dives/network.md`](./deep-dives/network.md))。
- **出站**:三档姿态(§2.4)——**公网默认放行**(经数据面 DNS/CONNECT/审计/限速/allow-deny 求值)、
  **内网私有段默认 deny-safe**(业务可显式放行)、**L0 地板恒 deny**(code 强制);承重墙是 dial 时对钉死
  IP 求值的 deny-set,域名 allow-list 只作 best-effort。provider 侧 nft 只放行到数据面 + 显式端点。
  数据面的**进程基数**(per-sandbox / per-node)是物化选择,**非模型承诺**:`local_runsc` 落成
  per-sandbox,别的物化可 per-node 复用一个数据面而仍按源 IP 强制 per-sandbox 策略。
- **入站**:默认关闭;**显式 publish** 把沙箱内端口暴露成外部入口,可带鉴权。publish 是**宿主侧
  规则**(nft DNAT)、纯加法、不触及容器,故可对运行中沙箱**热加**;而创建期绑定的网络策略变更
  走 resume 边界,与挂载/身份变更复用同一条路径(§5.C),是设计取舍而非技术限制。
- model 访问 = gateway 上一条带 provider 凭证的出站路由,鉴权挂稳定沙箱身份(§3.3),
  凭证留宿主侧。
- **凭证生命周期(三层分解收口)**:凭证走的是和网络策略**完全同构**的三层分解,本子系统是它
  的家、不散落无主——
  - **不变量**(支柱④,§2.1):provider/model 凭证**从不进 guest**,guest 只见代理别名。这是
    宿主隔离的一根支柱,与 provider 形态无关。
  - **策略权威**(本子系统):"哪条出站路由用哪份凭证、注入到哪个上游"由本子系统持有;和
    "出站 allow-deny / 入站 publish 规则"同属 §5.D 的策略权威。
  - **注入执行**(引擎自有 egress 数据面):凭证在出站时**由引擎数据面(gateway)注入**,经一条传输
    无关的 **resolve 契约**(alias → 注入 directives + 过期 + 失效信号)拿注入材料,数据面持 TTL 缓存。
    **provider 不进 resolve 路径**:它不解析别名、不调 resolver、不持求值权,即便凭证密文物理流经它
    调度的基础设施(远端形态),它也只是引流方、不是 resolve 参与方。resolve 契约的精确 schema 见
    [`deep-dives/network.md`](./deep-dives/network.md) D3.1 / [`open-decisions.md`](./open-decisions.md)。
    **resolve 失败 fail-closed**:注入材料拿不到时**拒绝该连接**(不 fail-open 原样转发——否则会把带
    alias 占位符的原始请求发给真上游,破支柱④),作用域 per-connection、不牵连其余 egress(§2.4;D5.6)。
  - 凭证明文**既不进控制面 Binding 记录、也不进 Overlay 工件**(§3.4 无第三类"可撤销外部授权";
    §2.3 凭证在"不可触及面")——它只存在于宿主侧一个独立后端。**这个后端(凭证怎么存/注册/轮转)
    是一类可插拔 seam,尚未定,登记在 [`open-decisions.md`](./open-decisions.md)。**
- **此处"鉴权"挂的是沙箱身份,不是用户/租户 principal**(后者仍是非目标,§1.4):publish 的
  `auth` 校验入站调用方、model 路由按沙箱身份注入凭证,都不需要引入授权主体概念。
- **不含**:宿主↔guest 控制通道(→ E)。

### 5.E Transport(宿主↔沙箱通道)

- **目标**:让宿主可靠地在沙箱里跑进程、读写文件,支持多连接并发。**engine 只暴露两个通用、
  无状态的 RPC 服务**,二者自足 = 一个 E2B 式沙箱;agent 会话语义不在 engine 内(见末条)。
- **in-guest daemon(`sandboxd`,核心组件)**:连接在 guest 内的终点是一个**payload 无关的
  daemon**,只暴露两个**通用**能力,且**对 agent / turn / session 一无所知**:
  1. **process** —— 起进程 / 重连 / 发输入 / 发信号 / 列进程,PTY 是带 pty 字段的特例
     (shell 只是 `exec /bin/bash` 带 PTY);进程按 pid 寻址(guest 原生)。
  2. **fs** —— `read`(可带 offset)/ `write` / `list` / `stat`,大文件走旁路;**不做 watch**
     (消费方用 `read`+`stat` 自行轮询拉增量)。
  这个 daemon 是"CLI/TUI/WebUI 都是同一套 API 的平级 client"(§6)成真的具体载体。它**必须保持
  通用**:不内嵌任何 agent/turn/session 语义,那些是 engine 外 adapter 的事(见末条)。
- **载体:netns 内 TCP,不用 vsock**(经 ECS + 本机实测):`sandboxd` 在 guest netns 里监听一个 TCP
  端口,宿主经该 netns 的 veth IP 直连——unary 走 HTTP、双向流走 WebSocket / h2c(带即时 flush)。
  gVisor userspace netstack 下:连接由 sentry 终结(实测 host dial 直达 in-guest listener)、unary 通、
  双向流保序且亚毫秒即时、1–4 MiB 大 payload 无死锁、冷启换新 netns IP(旧 IP 作废)零改动重连。**这直接
  兑现 §3.3「载体不编码 placement」**——重连只需拿沙箱当前 netns IP,不依赖任何固定地址。**那个 veth IP
  是引擎(placement,§5.A)分配的、不是 runsc 产物**;宿主如何拿到 IP 归 placement(§5.A:引擎自己分配即
  知道),不进本子系统。细节见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4。
- **无状态**:约束的是**控制面持久 SoT**——engine 不为连接维护持久记录,"沙箱上现在跑着什么进程" =
  实时 `process.List`、不落控制面注册表。可重启性不由 engine 承担:裸 pty 字节流明说 lossy(shell 本无
  持久 SoT),任何"不丢"担保由上层自理。transport 进程内瞬时、非权威的输出环形缓冲(供 `Connect` 回放 +
  慢订阅者非阻塞丢弃)不在此约束内,机制见 [`deep-dives/transport.md`](./deep-dives/transport.md) D4.3。
- **session 是 engine 外的纯加法**:agent 会话语义(turn / 历史 / 恢复,形态对齐 ACP——Zed 提出
  的 Agent Client Protocol,"coding agent 界的 LSP")**不在 engine 内实现**,而是 engine 外的
  adapter 叠在 `process`/`fs` 之上——
  turn 循环在进程流上识别、历史用 driver-native 落盘(`~/.claude` 的 JSONL,在 Overlay 工件里,
  经 `fs` 可读)、恢复调 agent 原生 `--resume`。engine 一个新概念都不加,这正是 §1.3「成本判据」
  与立足点④的兑现:**开放性在 adapter 层,engine 小而稳**。
- **引擎支持的"跑东西"只有一类:经 exec 拉起的进程**(monitor / 构建 / `apt`|`pip` 安装 /
  dev server / artifact server),**不以系统守护服务(`service`/`systemctl` 那套)为目标**。这条把
  "需要完整 init 机制"的整类需求移出范围(详见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md))。
- **不含**:agent / 会话 / turn 语义(→ engine 外 adapter)、网络策略(→ D)。

#### 5.E.1 guest 启动拓扑:`sandboxd` 的两个进程角色

`sandboxd` 是 guest 内核心组件,承担**两个正交职责**,靠**同一个二进制、两个进程**切开——不合并
(合并会让最复杂的 transport 崩溃时导致整个 guest 崩溃,也堵死热升级),不引第二个二进制(守 §1.1
单二进制部署)。runsc 以 `sandboxd` 为容器 entrypoint,**init 模式**启动当 PID 1,再经
`/proc/self/exe` **re-exec 自己进 transport 模式**当子进程:

```
PID 1 = sandboxd（init 模式：reaper + 信号转发 + subreaper 兜底 + 监督/重启）
  └── sandboxd（transport 模式：本节的 process/fs 终点，对 agent/turn/session 一无所知）
        ├── /bin/bash                 (process + pty)
        ├── agent 进程                 (process)
        └── apt / http.server / build  (exec 出来的子进程)
```

- **init 模式(PID 1)**:自研最小 init 语义(收割孤儿/僵尸、容器级信号扇出、监督并原地重启
  transport 模式),以 tini 源码为正确性 checklist,**不打包 tini 二进制**。它只当**兜底** reaper:
  transport 模式经 `PR_SET_CHILD_SUBREAPER` 声明自己为 subreaper,自己拉起的进程树的孤儿
  reparent 到它、由它收割,PID 1 只收真正的全局孤儿。
- **transport 模式(子进程)**:就是本节的 process/fs 终点,**保持纯通用**、对 agent/turn/session
  一无所知。**定向信号**(单进程/进程组,§4.2 协作式静止点)归它(它握 pid/pgid 句柄);**容器级
  信号扇出**归 PID 1。
- **收益**:`sandboxd` 崩溃降级成进程内可恢复(对齐 §4.2「崩溃 = 非自愿的 pause」),而非 substrate
  死亡;可热升级 transport 层而不打扰用户长跑进程;PID 1 信号语义与复杂 server 解耦。
- **代价:退出码跨 transport 重启 lossy。** `process.Wait` / 退出码保证**仅在 transport 单次存活期内**
  成立,跨一次重启退化为「`process.List` 仍可见、退出码可能丢」——与裸 pty 字节流 lossy 同源
  (engine 无状态,不丢担保由上层自理),不为它加内部状态。机制与 seam 见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.5。
- **常驻服务(若模板自带)与 transport 模式 `sandboxd` 平级**被 PID 1 托管,不塞进 `sandboxd`
  (§3.2.1 引擎一视同仁看待每个进程)。
- **resume 冷启时序**(init→re-exec→监听→重连的精确次序、探活在哪一步判 healthy)见
  [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D5。

> **细节另见 [`deep-dives/transport.md`](./deep-dives/transport.md)**:Connect-RPC 选型、process/fs wire
> 协议、session 划界与 seam、API 投影;**guest 启动拓扑的完整推理见
> [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4**。本节只明确边界,不展开细节(渐进式披露)。
