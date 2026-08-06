# 开放决策登记(Open Decisions)

> spec 只描述已收敛的架构;未拍板的选型、待细化的设计点、落地前要验证的假设登记在本文。`§X.Y` 是全局
> §编号,按 SPEC.md 顶部「文档地图」定位。
>
> **§1** 阻塞子系统的 spike · **§2** 架构已定、内部机制未展开 · **§3** 有候选未定论 · **§4** 实现缺口
> (设计已定、代码未兑现;有 owning plan 后迁入该 plan)。每条标注归属 + 当前倾向 + 谁消费;已在深挖
> 文档详述的只给一行摘要 + 指针,**不复制正文**。

---

## 1. 落地前验证(spike,阻塞相关子系统)

> 无。

---

## 2. 待细化的设计点(架构已定,内部机制未展开)

| 归属(plan §) | 待细化内容 | 备注 / 当前倾向 |
| --- | --- | --- |
| **§5.C Volume(契约边界外)** | 契约在 §5.C;待细化的边界项:独占卷抢占策略 / 直连变更 vs live-ro reader 准入 / snapshot **一致性标注**进 §6 / 远端 backend 落地形态 | **详见 [`deep-dives/volume.md`](./deep-dives/volume.md) D8**,本表不复制。多为产品层策略或落 §6 时定。抢占那条另有一条**锁序**论据(同文 D3:卷维度反查持有者会闭合成环,实测死锁) |
| **§4.4-C 事件流可重放窗口大小** | 事件流的对外形状已定(pull + `seq` 游标,§4.4-C / §6.1)。**仍开放的只有一项:可重放窗口取多大**,即何时开始裁剪 outbox。**详见 §4.4-C 末段**(为何是被推迟而非被回答、连带的 at-rest KNOWN LIMITATION),本表不复制 | 谁消费:未来外部 subscriber。窗口取值须由"承诺给外部订阅者的重连时长"论证,而今天无外部订阅者。加裁剪时调用方契约无需变更(410 已是 §6.1 保留语义) |
| **§5.D 凭证存储 seam** | 凭证"留宿主侧"的**后端**(secret-store):怎么存、注册、轮转。此处只留**at-rest 后端**选型;alias→directives 的 **resolve/缓存/失效契约**是另一条 seam,见下行 | 一类可插拔 seam(类比 runtime / rootfs provider);明文不进控制面/Overlay,只在宿主侧独立后端 |
| **§5.D egress 数据面 seam + 两条契约** | 引擎自有 egress 数据面(gateway,行为 seam)与两条传输无关薄契约的**精确签名**:① **resolve 契约**(数据面↔resolver:alias+目标上下文 → directives+过期+version 戳 + 失效信号)② **ingress 契约**(数据面↔引流:抽象描述符 + 双向劫持能力协商)。行为的两种强制模式(builtin 内联求值 / 外部编译下发)对接口要求不同 | **详见 [`deep-dives/network.md`](./deep-dives/network.md) D3/D7**,本表不复制。今天只建接口形状 + builtin 单实现(模式 1),registry plumbing 缓(D6);谁消费:§5.D 数据面 + 未来远端 provider |
| **§5.D 凭证注入模式** | builtin 首版支持哪些注入模式、各自约束 | **别过早钉 MITM 为唯一路径**;MITM 选中才需 guest 信任注入(CA 证书进 guest、私钥留数据面),落 §5.B 模板 / §5.E.1。**详见 [`deep-dives/network.md`](./deep-dives/network.md) D9**,本表不复制 |
| **§2.4 / §5.D egress deny-set 分层** | 三条 invariant 在 §2.3/§2.4(推理见 [`deep-dives/network.md`](./deep-dives/network.md) D5.3–D5.6)。**待细化的细粒度**:① L0 地板精确清单(orchestrator 自身端点 / sibling 网段如何在 placement 时算出;link-local/loopback 已定)② 内网 deny-safe 放行的 NetworkPolicy 语法 ③ fail-closed 是否发 §4.1 审计事件(倾向发;**容量压力事件不在此列——软阈已定为不发**,见 §4.4-C) | 谁消费:§5.A steering 物化 + §5.D 数据面 dial 逻辑 + placement;L0 清单是引擎资产、两跳共用一份 |
| **§2.4 / §5.D L0 地板怎么挣到(机制)+ deny-set 住哪** | **归 M8 一并决定。** 已定的是**问题**;未定的是机制:宿主端 veth / `netdev` 钩子的 `iif` 谓词、是否加 cap 收窄、是否夹 host forwarding,以及那份 placement 派生的 L0 清单**住在哪**(候选:mediator 按既有 keep-set 形状算出后分发给两跳——leaf 包不合适,因为 leaf 是给**静态**共享事实的,而这份清单要在运行时被写) | 谁消费:M8。**推迟的代价是明写的**:在 M8 落地前 §2.3 的 sibling-不可达与 §2.4 三条 invariant **都不成立**,见 §4 实现缺口 |
| **§5.A provider 接口(远端形态入参)** | **待细化的是远端形态的入参形状**:`RootfsSource`(本地 `MergedPath` vs 远端句柄)、`SteeringSpec`(egress 引流描述符)、`ResourceLimits` 在非 cgroup 调度层的等价物——用"被调度的远端隔离单元"作纸面参照反推,**不实现任何远端 provider** | 谁消费:未来远端 provider |
| **§5.B Overlay 物化** | 待细化:content-addressed diff 层、multi-node 经 rootfs provider 跨 node 寻址(**应用层协作安全点不在此列——已否决**,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D4.4) | **详见 [`deep-dives/rootfs-overlay.md`](./deep-dives/rootfs-overlay.md) D5**,本表不复制 |
| **§5.C per-volume 容量帽在 remote 后端的物化** | local 臂是 subvolume + squota(§5.B/§5.C 共享原语)。**待细化的是 remote 后端**:一个对象存储 / 网络 FS backend 用什么手段兑现"这个卷最多多大"(后端配额 API / 客户端侧核算),以及它与 `Capabilities()` 的关系 | 封在该 backend 内(volume.md D1/D7),不进核心;谁消费:未来 remote volume backend |
| **§5.A / 模板(内容目录)** | **内置 Template 的内容清单**:一个 bare base(uv/pip/git/build-essential;可选 sudo,仅作 guest 内便利、非引擎语义),各 agent 栈(claude-code/opencode/pi)叠在 bare base 之上。**这是产品/模板层的内容选型**,仍开放(Template 的一等行化 / boot seed / multi-lowerdir 叠层机制是 §4.3 / §5.B 的既定架构,不在本行) | guest 默认 root(§2.2),装包是一次性 exec、不引入 init 依赖,见 [`deep-dives/guest-runtime.md`](./deep-dives/guest-runtime.md) D3.1 |

---

## 3. 待拍板的选型(有候选,未定论)

> 无。

---

## 4. 实现缺口(设计已定,代码未落地 / 待收口)

| 缺口 | 现状 | owning plan |
| --- | --- | --- |
| **§5.D 网络策略强制(支柱③ 的 enforcement)** | **设计已定(§2.4 三条 invariant / §5.D / network.md),代码一行未落**:无 `internal/network` 包,无 nft / TPROXY / deny-set。今天 `local_runsc` 只建 netns + veth + 两个 `/30`,**egress 不被强制**——沙箱当前到不了外网/兄弟/link-local,**仅因为没有默认路由**,而 guest root 持 `CAP_NET_ADMIN`、宿主 `ip_forward=1`,故一条 `ip route add default` 即可达宿主 LAN 与兄弟沙箱 IP(实测)。§2.4 三条 invariant 今天**无一成立**;宿主 loopback 与 orchestrator 控制面是被 `127.0.0.1` bind + `route_localnet=0` **偶然**覆盖的,不是被强制。**连带**:`GET /v1/sandboxes/{id}/network` / `POST …/publish` 两个端点 404,`POST /v1/sandboxes` 的 `network?` 键被静默丢弃(未知键)。**L0 地板怎么挣到、deny-set 住哪**两项是设计决策,见 §2 对应行 | **M8**(`docs/plans/M8-network.md`,待建) |

> **为何登记在此而非留白**:本表的作用正是回答"哪些已定但还不真"。§5.D 是最大的未建面、且是安全关键面,
> 而 `SPEC.md` §2.1③ / §2.4 是用**现在时**写的——一个只读 spec 的人会以为它已被强制。§5.D 与冷启链
> (guest-runtime D5.1 的 ③)各自带一条同样的未建标记。本行在 M8 落地后移除。
>
> **M8 必须带上的实测**(在 `sbx` 上量的,别重新发现一遍):
>
> 三条已写进 [`deep-dives/network.md`](./deep-dives/network.md) D5.5,**本表不复制,读那里**:
> ① 点对点 `/30` veth 不隔离 sibling(宿主在两段之间路由),故 nft 地板是强制而非兜底;
> ② 沙箱 netns 内的 nft 看不到 guest 报文,故 enforcement 只能挂宿主端 veth 或 `netdev` 钩子,
> **netns 内的 TPROXY 不成立**;③ `saddr` 谓词可被 guest 伪造、`iif` 谓词不可。
>
> **第四条只在本表有,别丢:逃逸需要两步,当前的"安全"是两个巧合。**
> `ip route add default via <host> dev <veth>`(**要带 `dev`**,不带则 rc=0 但不通)+ 宿主侧 SNAT
> 存在,才真正出网;今天不通是"没有默认路由"**且**"没有 SNAT"两件事一起,不是任何一条强制。

---

> **纪律:** 任何一项拍板后,先更新对应深挖文档或 plan 的相应章节,**再从本登记移除**——保证
> "已定"进 plan、"未定"留本文,两边不重叠。§4 的缺口同理:owning plan 出现即迁入、从此移除。
>
> **引用本表按标题,不按位置。** 每一行的**加粗标题**就是它的名字(§2/§3 用 §编号,§4 用缺口名),行内
> 不编号。理由是机制性的:收口一项就要删一行,而删行会把它下面所有行的序号平移——于是别处"§4 ②"这类
> 引用**无声**改指另一条无关的项,删除方那边不会有任何报错。按标题引用则随收口一起失配,断得见。
