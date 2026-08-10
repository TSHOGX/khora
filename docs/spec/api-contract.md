# Sandbox Engine — API 与 CLI 契约(§6)

> 属 [SPEC.md](./SPEC.md);全局 §编号跨文件连续,导航见 SPEC.md 顶部「文档地图」。

---

## 6. API 与 CLI 契约

后端只暴露 **sandbox-centric** 的 API,**全 ID 寻址**;产品概念(session / mode 等)不进
后端。**CLI 与前端是同一套 API 的平级 client**,CLI 是一等公民,不是后门。

### 6.1 资源与端点(控制面 REST + 数据面 Connect-RPC,分工见 [`deep-dives/transport.md`](./deep-dives/transport.md) D5)

> **外沿协议分工**:沙箱/template/volume/network **生命周期 = REST + JSON**(控制面块,权威端点,
> OpenAPI 事后描述);**process / fs 数据面 = Connect-RPC**(`engine.transport.v1`,权威契约是
> `transport.proto`,client 经 `buf` 生成 stub。该 package 下是**三个**服务:对外的 `ProcessService` /
> `FsService`,外加引擎自用的 `HealthService`——后者不是对外能力面,见 §5.E)。client 一律经
> `orchestrator` 居中、不直连
> `sandboxd`(§4 mediator / D5.1)。数据面块的 REST 行**仅作形状示意**,不是权威端点(D5.3)。
>
> **这套 API 自身不做认证/授权**:引擎内无授权主体(§1.4 非目标),故 §6 假定调用方已被信任。谁能调用
> 由**引擎外**决定(部署把它绑在本机/私网,或前置一层网关),那是 join table 的加法、不是引擎原语。

**控制面(REST + JSON,权威端点)** —— 沙箱 / template / volume / network 生命周期,外加事件流重放。
**全是低频请求-响应,无长连接、无 server-push**:每条都是一来一回的 CRUD 语义,故连事件流也是
**游标分页 GET**(`/v1/events`,见下)而非 SSE/websocket —— 引擎不持有任何 per-subscriber 状态
(§1.3),这正是它能留在控制面的原因。**"无流"指的是没有推送面,不是没有事件面。**

```text
# Sandbox 生命周期
POST   /v1/sandboxes                 {template_id, network?, volumes?, idle_timeout?, retention?,
                                      cpu_millis?, memory_byte?, disk_byte?}        → {sandbox_id}
#   `network?` = §5.D 的四字段声明,schema 见下方 Network 块(**给了就必须含 `egress.default`**)。
#   **整份省略是合法的,归一化成 `{deny, [], [], []}` 并显式落库**(§5.D):故"省略 network"与"给了
#   network 却漏 egress.default → 400"是两件不同的事,后者才是 §6.2 那张清单的第一类。
#   资源上限三轴见 §3.4(Binding);按轴条件必填的规则见 §6.2。准入不足 → 507。
#   `volumes` 每项 = {volume_id, path, access},`access` ∈ {exclusive, shared-ro, shared-rw} —— 它是
#   **挂载行**的属性(§5.C),故在此给出而非在卷记录上。撞独占租约 → 409(§6.2 四个 409 之 ③)。
GET    /v1/sandboxes/{id}                                     → {sandbox_id, state, death_cause?}
#   只读生命周期态 + 死因(§4.3)。`death_cause` ∈ {`gone`, `resource-exceeded`, `unknown`},
#   **未观测到死亡时该键不出现**(不是空串、不是 null):`CauseNone` 是"没有死因"而非一种死因,
#   序列化成 `""` 会在健康响应上摆一个 falsy 却存在的字段、诱人拿它当存活判据。
#   **为何上读面**:§4.3 把恢复决定判给调用方(dead-recoverable → resume;resource-terminated →
#   不 resume),调用方必须能直接读到它所依据的事实,而不是先发一个必被拒的 resume、从 409 错误串里反推。
#   值是 §4.3 的**抽象枚举**,绝不是 errno / cgroup 路径——远端 provider 产不出宿主原语(§5.A)。
PATCH  /v1/sandboxes/{id}            {cpu_millis?, memory_byte?, disk_byte?, idle_timeout?, retention?,
                                      volumes?, network?}
                                     → {sandbox_id, state, applies_at, lease_released?}
#   改 Binding,生效点与可改状态见 §3.4。**wire 语义与 create 不同**:这里"未给"与"给 0"必须可区分
#   (帽的 0 = 无上限、`retention: 0` = 不驱逐),故请求字段**可空**——未给 = 不动该字段。TTL 收
#   duration 字符串("48h")。`applies_at` ∈ {"now"(仅改 TTL), "next-resume"(改了任一资源上限
#   **或挂载绑定、或网络声明**)}。
#   **`volumes` 是挂载表那份完整绑定**(每项 {volume_id, path, access});§6.2 已把 PATCH 定为改绑定点,
#   故它在此列出而非另开 attach/detach 端点。**它的生效点分裂成两半,`applies_at` 一个值答不了**:
#   挂载拓扑变更在 **next-resume** 才物化(runsc 启动时取挂载,§5.C 不热挂),而 **detach 立即释放独占
#   租约**(租约就是那条挂载行,行一删租约即释放)。故 detach 的响应是 `applies_at: "next-resume"`
#   **且 `lease_released: true`** —— 两件事不同步是设计事实,不是可以折叠成一个值的细节。attach 到已被占用
#   的卷 → 409(§5.C 原子插入的 `RowsAffected()==0`)。
#   对 `running` 改帽 → 409「先 pause」;抬上限重过准入门(可 507 / 400)。不发 §4.1 事件(非状态转换)。
#   **`disk_byte` 的 `0 → N` 被拒(400)**:无帽沙箱的 upper 上没有可写的配额行,故这次写会声明一个
#   每次 placement 都兑现不了的帽,接受它等于把沙箱变成砖(完整推理与为何拒在写点见 §3.4)。
#   `N → 0` 撤帽与 `N → M` 改帽都合法。**⚠ `N → M` 的写点未收口**:降到当前用量以下今天无校验、返 200
#   并卡死该存储单元(卷侧已落,见 `PATCH /v1/volumes/{id}`)。缺口与所需的 §5.B seam 加法见
#   [`open-decisions.md`](./open-decisions.md) 实现缺口表。
#   **`network` 同为整份换整份**(§5.D 声明是 Binding),生效点 `next-resume`,故与资源上限同规则:
#   对 `running` 沙箱改 → 409「先 pause」。**publish 不走这里**——它有自己的热加端点(见 Network 块)。
#   **故改 `network` 必须 read-modify-write:先 GET `…/network`,在读回的那份上改,再整份回写。** 两个写点
#   写同一份声明,而 PATCH 换整份:拿一份读旧了的声明回写,会把此间热加的 publish **静默丢掉**。引擎不做
#   字段级合并、也不给一个乐观并发戳去检测这种冲突,最后一次 PATCH 就是结果——一个安全相关字段上的
#   lost update,故调用方侧那次 GET 是契约要求而非建议。**今天没有 un-publish 端点**:撤一条 publish
#   就是整份 `PATCH`(少写那一项),按 Binding 规则在 `next-resume` 生效。
POST   /v1/sandboxes/{id}/pause
POST   /v1/sandboxes/{id}/resume
#   resource-terminated 的沙箱拒绝 resume → 409(§4.3;先 PATCH 抬帽)
DELETE /v1/sandboxes/{id}                                                          (destroy)
GET    /v1/sandboxes                                   (list;每项与单读同形,含 `death_cause?`)
#   list 也带死因是**为省一轮 N+1**:§6.2 全 ID 寻址,若不带,调用方见到若干 `paused` 就得逐个
#   GET 才知道哪些可 resume —— 而那条事实 list 本来就持有。
GET       /v1/sandboxes/{id}/files?path=                                 (大文件下载,流式)
PUT|POST  /v1/sandboxes/{id}/files?path=                       → {path, bytes_written}(上传)
#   `POST` 与 `PUT` 同义(上传不是幂等替换语义之争,收两个动词省调用方一次适配)。
#   §5.E fs 的大文件旁路(D4.4),数据面里唯一走 REST 的一条;小载荷仍走 `FsService.Read/Write`。见下。

# 事件流重放(§4.1 转换事件 / §4.4-C outbox);**只做 pull,无推送面**
GET    /v1/events?after=<seq>&limit=<n>     → {events:[{seq,id,sandbox_id,kind,state,occurred_at}],
                                               earliest, latest}
#   `seq` 是**游标**:把收到的最后一个 `seq` 原样作下次 `after` 传回即续位。`after` 省略/0 = 从
#   保留窗口起点。**排序键是 `seq`,不是 `occurred_at`**(后者不是全序,理由见 §4.4-C)。
#   `limit` 有**服务端硬上限**(默认 100、上限 1000),超限**截到上限**而非拒绝。硬上限而非仅默认值:
#   参数由调用方控制,只给默认值等于没给。且**今天不裁剪 ⟹ 窗口 = 全部历史**,`after=0` 要翻的行只增
#   不减 —— 不裁剪**加重**而非缓解无界 backlog 风险,这才是硬上限为必做项的原因。(窗口取多大仍开放,
#   见 §4.4-C / [`open-decisions.md`](./open-decisions.md) 该文 §2「§4.4-C 事件流可重放窗口大小」;裁剪一旦
#   加上,调用方契约不变——见下条 410。)
#   `{earliest, latest}` 是保留窗口边界(outbox 空时 0/0)。
#   **410 是保留语义,今天不可达、不实现**:不裁剪 → `earliest` 永不前进,故"`after` 早于窗口"
#   这个态构造不出来。写在契约里是为了**将来真加裁剪时调用方契约无需变更**——届时 `after < earliest`
#   即 410,调用方须从 `earliest` 重新起。**不为不可达态写代码**(§1.3)。
#   重放**按保留窗口,不按投递状态**:引擎内那个 sink 有没有收过某条,不是外部 subscriber 该依赖的事实。
#   **为何在控制面**:游标分页 GET 就是 CRUD;且引擎级事件流既非 per-sandbox 也非 per-pid,数据面
#   本就无处寻址它(§6.2)。**为何只做 pull**、以及 push 为何是零代价的适配器:§4.4-C。

# Template(不可变工件:opaque ID 作身份,存在即状态,§3.1 / §4.3)
GET    /v1/templates
POST   /v1/templates                 {from_sandbox_id}  (Promote,§3.1)          → {template_id}
#   源须是 `running` 或 `paused`(否则 409)。`running` 的源被静止到 `paused` 后固化(照发一条普通
#   `paused` 事件),`paused` 的源原地固化。过 §3.4 准入门(代价预报不足 → 507),且门在静止**之前**,
#   故一次被拒的 Promote 不是一次可观测的转换。
DELETE /v1/templates/{id}            (被沙箱/子层引用时 → 409 template in use;引用-回收原子性 §4.3)

# Volume(与沙箱生命周期解耦)
POST   /v1/volumes                   {size?}                                  → {volume_id, size}
#   **无 `access_mode`**:它是挂载行的属性、不是卷的属性(一个卷可同时对 A 是 shared-rw、对 C 是
#   shared-ro,§5.C),故只出现在 create/PATCH sandbox 的 `volumes` 项里。`size` 在 disk 轴被计量时
#   **必填**(§3.4 / §6.2 按轴条件必填),否则 400。
#   **无 `backend` 入参**:后端是部署期注入的单一实现(与 store / runtime provider 同形,§1.3 注册
#   而非选择),故它只出现在**响应**里,不是请求键。
GET    /v1/volumes                                                                 (list)
GET    /v1/volumes/{id}              → {volume_id, size, backend?, used_byte?}     (inspect)
#   `size` 是**记录里的帽**(唯一权威:后端读回分不清"从未设帽"与"故意无帽",volume.md D7.3);
#   `used_byte` 是后端自陈的已用字节,**读不到时该键不出现**(不报 0——0 会被读成"空",而调用方要拿
#   它选一个严格大于它的新帽)。list 不带 `used_byte`(每卷一次后端往返,list 上乘以卷数)。
PATCH  /v1/volumes/{id}              {size}  (**必填**,它是唯一可编辑字段)       → {volume_id, size}
#   卷帽的唯一写点(create 之外),`size` **必填**。**未挂载与挂载中都可改**,故无"先 pause"的 409——
#   与沙箱资源上限只在 `paused` 可改相反,机制性理由见 §5.C。抬帽重过准入门(可 507);**降到低于当前
#   用量 → 400**(严格不等式,§5.C / volume.md D7.3)。改帽不受写入 regime 管,但改 `access` 另有规则
#   (§5.C)。不发事件。
DELETE /v1/volumes/{id}
#   两种 409(§6.2 四个中的 ①② 卷侧):有活挂载行 → 409(被引用);**有活快照/克隆(子代卷)→ 409**
#   (lineage RESTRICT,与 Template 被子层引用同形)。两者是不同的对象关系,故 sentinel 与文案必须可区分。
POST   /v1/volumes/{id}/snapshot     {size?}                                  → {volume_id, size}
POST   /v1/volumes/{id}/clone        {size?}                                  → {volume_id, size}
#   **两者都收 `size`,且 disk 被计量时必填(缺失 → 400),不继承源的帽**——基底没有可继承之物,而
#   策略侧继承会静默把节点已声明的磁盘主张翻倍(§5.C / volume.md D6.1a)。
#   产出的是**普通可写卷**(无只读标记;要不可变用 `shared-ro` 挂载行),记录带 `parent` 指向源。
#   新卷的帽同样过准入门(可 507)。后端不支持 → 400,且 capability 必须在分配 ID / 动字节**之前**问。
POST|PUT /v1/volumes/{id}/import     ?path=  (直连写入,body 即字节流;无 live-rw 挂载时才放行)
                                             → {volume_id, path, bytes_written}
GET    /v1/volumes/{id}/export       ?path=  (直连读出)
#   **不需要沙箱**——这是"卷与沙箱生命周期解耦"的兑现面:**从零播种**(还没有任何沙箱)与**读最后一个
#   沙箱消失之后的卷**都走这里。`path` 相对卷根;越界不可达(seam 的 `OpenRoot` 是根限定的,§5.C / D5.1)。
#   有 live-rw 挂载持有该卷 → **409**(写入 regime;`paused` 沙箱**不算** live writer,故此时放行——
#   谓词与 in-flight 标记的临界区要求见 §5.C)。字节流式,不整块缓冲。

# Network(声明在 create 时绑定,改声明走 resume 边界;publish 是显式入站例外,可热加)
GET    /v1/sandboxes/{id}/network    → {state, egress:{default, allow[], via[]}, ingress:{publish[]}}
#   读回那份声明(§5.D 的四字段)。**三个数组恒出现,空就写 `[]`,不省略、不 null**:它们是记录里确定的
#   取值,而 `death_cause` / `used_byte` 那条省略规则治的是**读不到的事实**,声明没有读不到的情形。
#   `ingress.publish[]` 每项 = {guest_port, proto, host_endpoint?}。**`host_endpoint` 在引擎不持有活
#   placement 时该键不出现**(不是空串、不是 null):端点是 placement 的产物(§3.3),`paused` 沙箱根本没有
#   端点可报。理由与 `death_cause` 逐字同一条——一个 falsy 却存在的字段会诱人拿它当"这条 publish 是否生效"
#   的判据;**而那个判据本身错位**:规则集整份随 placement 原子装上(§2.4),没有 per-publish 的安装态,
#   故 per-项的有无只是把同一个 bit 复述 N 遍。endpoint 随 placement 变,不得当稳定标识缓存(§3.3)。
#   **`state` 是本响应的成员**,与 `GET /v1/sandboxes/{id}` 同一取值、同一次 store 读,省调用方一轮
#   "读回来还得再取一次态才能解释它"(同 list 带 `death_cause` 那条省 N+1 的判据):
#   **`running` ⟺ 内核里装的那份就是本响应里的这份声明;非 `running` ⟹ 什么都没装。**
#   这个双条件是 §5.D + §3.4 的机制结论、不是本端点的新承诺:声明每次 placement 按当前记录重新生成,而两个
#   写点都不能让一个 `running` 沙箱的记录与它活着的规则集分叉(改出站三字段对 `running` 判 409;publish 把
#   "落库"与"装规则"做成一次不可半成的写)。**故"已声明"与"已生效"之间不存在可持久的差**,读面要暴露的只有
#   "有没有 placement"这一个 bit。**因此不加 `in_effect` / `pending` / `applies_at`**:① `in_effect` 是
#   `state` 的第二种拼法且**越权**——引擎能诚实报"我持有一个 placement",不能报"内核里此刻真有这几条规则"
#   (一个隔离单元已死而尚未被 §4.3 降级的 `running` 记录就是反例);② `pending` 要求记录里有第二份声明,
#   而 §6.2 明写绑定"没有待定/未解析值",且 `paused` 那份不是待定、它就是声明本身;③ `applies_at` 是**写**
#   响应的词汇,读面没有那次编辑,硬给一个也只是 `state` 的函数,还让同一个键在两个端点上意思不同。
#   **本响应的响应体就是一份合法的 `PATCH` 请求体**——读面吐出的只读成员(`state`、以及 `publish[]` 每项的
#   `host_endpoint`)在写面被**收下并忽略**。理由不是便利:上方 PATCH 注把 read-modify-write 定成**契约要求**
#   (安全相关字段上的 lost update),而三个写点都拒未知键,故若不收,一个把 GET 响应原样回写的调用方会因
#   **回显引擎自己刚吐出的键**被 400——摩擦全落在那条承重路径上,把调用方推向 §5.D 点名危险的那条路。
#   规则是「**读面吐出的每一个只读成员**」而不是「`state` 这一个」:拒未知键**递归生效**,故漏掉嵌在
#   `publish[]` 里的 `host_endpoint` 会让一个带 publish 的 `running` 沙箱的响应体在低一层被拒。
#   **而那道 400 也不许当次序护栏留着**:它只在 §5.D 点名危险的那个次序上出现(安全次序
#   `pause → GET → PATCH → resume` 的 GET 排在 pause 之后、无 placement 故不吐 endpoint),等于把一条
#   并发次序要求藏进一个 JSON schema 错误里报;次序要求由 §5.D 以文字承载。
#   **这不是给"拒未知键"开的普遍例外**:只有读面自己产生的只读成员在此列,拼错的 `netwrok`(以及 publish
#   项里拼错的 `host_endpiont`)照旧 400。读面将来多一个只读成员,写面就相应多一个被忽略的成员。
#   非 GET → **405**(§6.2 通则,不是落进 404)。
POST   /v1/sandboxes/{id}/publish    {guest_port, proto}
#                        → {guest_port, proto, host_endpoint?, applies_at}
#   纯宿主侧规则、纯加法、不触及隔离单元,故**可对运行中沙箱热加**(与出站声明走 resume 边界相反,§5.D)。
#   **`running` 与 `paused` 都合法、都 200**,差别只在生效点:`running` 当场装规则(`applies_at: "now"`),
#   `paused` 只落库、规则在下一次 placement 出现(`applies_at: "next-resume"`,且**无 `host_endpoint` 键**)。
#   **响应照 `GET …/network` 那条省略规则**——写面与读面在同一字段上分叉即契约缺陷。为何 `paused` 合法、
#   以及"拒 `paused` 反而把调用方赶上更不安全的整份 `PATCH`"见 §5.D。`creating` / `destroyed` → 409
#   (动词的源态规则);未知 ID → 404。
#   **重复 publish 同一 `{guest_port, proto}` 幂等**:返既有那条、不追加第二项(去重必须在声明层,基底不会
#   替引擎发现它,§5.D)。**不发 §4.1 事件**(非状态转换)、**不动 `UpdatedAt`**(`retention` 从那列起算)。
#   `proto` 收 tcp 与 **udp**——`via[]` 那条 UDP 排除的机制理由在入站方向不成立(§6.2)。
#   **它落库**:热加的是规则,写下的是声明,故这一条跨 resume 存活(声明是 Binding,每次 placement 按
#   当前记录重装规则集,§5.D)。因此它是 `ingress.publish[]` 上**第二个写点**,与 PATCH 的整份换整份
#   相互作用——read-modify-write 的要求与"无 un-publish 端点"见上方 PATCH 注。
#   **无鉴权键**:校验一个入站调用方必然要终结并理解应用层协议,而引擎不贡献这样的进程(§5.D)。
#   引擎交付一个 `host ip:port`,前面摆什么(TLS 终结 / hostname / 证书 / 调用方校验)都是引擎外的事。
#   **要求 guest 侧绑 `0.0.0.0`**:沙箱有独立 netns 与独立 loopback,故绑 `127.0.0.1` 的服务 publish
#   不到——契约是**连接被拒且快速失败**(不是挂到超时),**不承诺可归因**:实测它与"guest 里没有监听者"
#   在调用方侧都是同一个 `connection refused`、不可区分(§5.D 四条用户可见语义之 ① / network.md D5 ⑯)。
```

> **`network?` 的 schema 就是 §5.D 那四个字段,全部只用 IP / 端口 / 协议词汇**——`egress.default`
> (`deny|allow`,**必填**)· `egress.allow[]` · `egress.via[]` · `ingress.publish[]`。三处元组一律是
> `{cidr, port, proto}`,`proto` **恒必填**;`cidr` **只收 IP 字面量**。**`cidr` 与 `port` 都可省,省略即
> 通配**(任意目的 / 任意端口,§5.D),而**读面把通配渲染成"省略"而不是 `"port": 0`**——那是"读面输出必须
> 是合法写体"这条(上方 PATCH 注)在这个字段上的具体要求。以上必填与收窄的理由在 §5.D
> (机制侧见 network.md D2 / D3.1),违反它们的**状态码归 §6.2** 那张清单。**没有"姿态"枚举**:姿态是
> 这四个字段的取值组合(§5.D)。
> 声明是 Binding(§3.4),故它跟资源上限一样**只在 `paused` 可改**、经 `PATCH /v1/sandboxes/{id}`,
> `applies_at: "next-resume"`;**唯一例外是 publish**,它有自己的热加端点(上面那条)。

> **为何 files 旁路走 REST 而不走 RPC:它的全部意义是流式。** 大文件不该变成一条 protobuf 消息、也不该在
> 任一端整份驻留——实测 RPC 路径多占约 37 倍 guest 内存(transport D4.4),而沙箱超 RAM 帽即被终止
> (§3.4),故这是安全性而非效率取舍。**阈值由调用方定**。§6.2 的约束不变:沙箱仍按不透明 ID 寻址,
> guest 端点(placement)不出现在 wire 上。

**数据面(Connect-RPC,权威 wire 是 proto,非下方 REST 行)** —— process / fs 有 server-stream +
强类型帧,**权威契约是 `engine.transport.v1` 的 `ProcessService` / `FsService`**(D4.3/D4.4),
client 从 `transport.proto` 经 `buf` 生成 stub、经 `orchestrator` 反代到 `sandboxd`(D5.1)。**沙箱寻址
走带外请求头 `Engine-Sandbox-Id`**:proto 本身只按 guest-native pid 寻址(D4.6 词汇边界),反代 strip
掉该头、在引擎内解析当前端点(placement 永不外泄,§6.2 / D5.4)。下方 REST 行**仅作形状示意**(说明
有哪些操作、如何 pid/path 寻址),**不是可实现的权威端点**——真正的 wire 看 proto:

```text
# Process(连接)— 起进程 / 重连 / 输入 / 信号 / 列进程;PTY 是带 pty 字段的特例,pid 寻址
#   权威:ProcessService.{Start,Connect,StreamInput,SendInput,SendSignal,Update,List,CloseStdin}
(示意)  processes                     {cmd, args?, pty?, cwd?, env?}   → {pid, stream}
(示意)  processes/{pid}/connect                                                     → {stream}(重连,先回放有界环形缓冲再接 live,D4.3)
(示意)  processes/{pid}/signal        {signal}
(示意)  processes                                                                    (实时 list,无注册表)

# FS(读写沙箱文件)— read 可带 offset,无 watch;语义与生效点见 D4.4
#   权威:FsService.{Read,Write,Stat,List,MakeDir,Remove,Move}
(示意)  fs?path=&offset=&len=                                                        (read / stat)
(示意)  fs?path=                                                                     (write)
(示意)  fs/list                       {path, depth}
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
  未计量轴上合法);缺失即拒(400),这与"节点已满"(507)是两件不同的事。这是 §6.1 **两条**取决于部署
  配置的字段语义**之一**(另一条是下面网络声明那条**部署性质的 400**:`via[]` 遇上未给第二跳 uid 的部署),
  故两条都显式写在此处而非留给实现。
- **状态码**:准入容量不足 → **507**(说的是"容量",而非 409 那种调用方可重试的状态冲突);
  `resource-terminated` 拒绝 resume → **409**;计量轴缺帽 / 请求体不合法(负的帽、无法解析的 duration)
  → **400**;被引用的 Template 删除 → **409**;源态不合法的动词(如对 `running` 改帽、对非 `paused`
  沙箱 resume)→ **409**;未知 sandbox / template ID → **404**。
  **另外三个通则码**:pause 正在 drain 时的写操作(fs 变更 RPC / files 上传)→ **503**(§4.2 barrier:
  拒绝而不排队,排队会把 drain 饿死;可重试);files 上传超引擎单请求上限(8 GiB)→ **413**;任何
  集合/对象路由上的错方法 → **405**。
  **成功码**:create / Promote / snapshot / clone → **201**;DELETE(sandbox / template / volume)
  → **204**;其余读与动词 → **200**。
  **Volume 的四个 409 语义不同、须可区分**:① 删一个有活挂载行的卷(被引用,同 Template 那条);
  ② 删一个**被活的引用行挡住的对象**——它有**两侧**,卷侧(删有活子代卷的源,lineage RESTRICT,§5.C 快照)
  与沙箱侧(删一个有活挂载行的沙箱,`mounts.sandbox` RESTRICT);③ attach 撞独占租约(可重试的状态冲突);
  ④ 有 live-rw 挂载时的直连变更(§5.C 写入 regime)。
  **四个是"结局类别"的计数,不是 sentinel 的计数**:②④ 各自内部还要再分,因为**同一个类别可以从两侧被触发,
  而两侧要报不同的对象**——② 的两侧是两条不同的 FK 路径(卷侧的 lineage RESTRICT vs 沙箱侧的
  `mounts.sandbox` RESTRICT),④ 的两侧是写入 regime 的两个方向(直连变更撞上活的 rw 挂载 vs 冷启动要
  rw 挂载一个正有直连写在飞的卷),故各有自己的 sentinel——**六个 sentinel 覆盖四个类别**。"四个不增加"
  约束的是**调用方要分辨的语义种类**,不是实现里的错误值个数;新增一条触发路径若落在既有类别里,
  加 sentinel 不算增加类别。
  ①②③在 store 侧是两类不同的约束违反(FK-RESTRICT vs 唯一/谓词失配),故**翻译点要给各自的 sentinel**,
  不能共用一个"in use"文案——既有 `ErrInUse` 的文案写着 "template in use",复用它会返回一个说错对象的 409。
  **另有一条不属于这四类**:源态不合法的动词(对 `running` 改帽、对非 `paused` 沙箱 resume、以及非法源态的
  改绑定)本就按上文通则判 409,它是**动词的源态规则**、不是 Volume 的引用关系,故不进这个枚举。
  **幂等删除不得吞掉这些 409**:它们是 RESTRICT 触发的**全部**对外信号,若落进"不存在也返成功"的分支,
  守卫失败的删除就与成功的删除不可区分(外部反面教材见 [`deep-dives/volume.md`](./deep-dives/volume.md) D9)。
  **网络声明的九类 400 + 一类部署 400**(§5.D。**九类声明 400** 全部是"不读别的行即可判定、且无重试能让它
  合法",同挂载绑定那条判据;**第一类只在 `network` 对象存在时求值——整份省略是合法的,归一化成
  `{deny, [], [], []}` 落库**,§5.D):
  `egress.default` 缺失 · `cidr` 处给了域名 · 元组缺 `proto` · `via[].match` 匹配 UDP ·
  **`via[].endpoint` 非宿主本地** · **`via[].endpoint` 落在 L0 地板的 daddr 清单项内**(即 link-local /
  loopback / sibling CIDR)· **`egress.allow[]` 的 `cidr` 整段落在地板"任何读法下都抬不动"的项内** ·
  **`cidr` 处给了 v4-mapped / v4-compatible 的 v6 字面量** · **良构性:声明对象自身立不起来**(与其中任何
  取值无关)——`via[]` 条目的 `match[]` **为空**,或 `publish[]` 条目的 `guest_port` **为 0**。
  **另有一类不属于这九类、性质是部署而非声明文本的 400**:`via[]` 声明遇上**未给第二跳 uid 的部署**
  (见下方那段"另有一条网络 400 不属于上面那张清单")。
  **第九类(良构性)为何是独立一类,而不是第三类下面多几个错误值**:判据就是上面 Volume 那条——**类的基数
  约束的是"调用方要分辨的语义种类",不是实现里的错误值个数**。第三类的 remedy 是"在这个元组里写上
  `proto`"(一个字段、一次编辑,且报错点名了那个字段);而这两样要求调用方**给数组添一个成员**
  (`match[]` 根本没有条目,故也就没有元组可供 `proto` 缺失)或**补一个请求里从未提到的端口**。把它们报成
  "元组缺 `proto`",是把调用方送去检查一个可能写得完全正确、甚至尚不存在的字段。**而这两样彼此合为一类**:
  它们是同一个语义种类("形状立不起来"),remedy 也是同一类编辑(补上结构上必需的成员),分开就成了数
  实现里的错误值。**不属于良构性的是"元组没写 `port`"**——那是**合法的,意为任意端口**(§5.D):省略通配是
  一条被声明的语义、不是缺成员,而 `match[]` 为空在任何读法下都引不走任何流量。
  前六类里的第五、六类把 endpoint 夹成"宿主本地、
  且不在地板的 **daddr 清单项**内"(**"宿主本地"必然落在地板的"宿主自身的任一地址"项里,那不是矛盾**:
  开洞靠次序、地板不被收窄,§2.4 那段边界),两者是同一个判据的两面:第二跳的规则由内核压在同一份宿主规则集
  后面,而内核只压得住本机
  进程——收下一个远端 endpoint 就等于把地板架在那个进程自己的自律上(confused deputy,§2.4 第二条明确拒的
  东西);而落在地板内的 endpoint 是「声明抬不动地板」的直接后果,它让每一条命中流量**永久 fail-closed**。
  两者都是**可证明永不能工作的声明**,故在写点当场拒(推理见 network.md D4.2 / D4.3)。**第六类只读地板的
  daddr 清单项,不读"宿主自身"那一项**:后者是**一条 ct-state 规则而非地址枚举**,把它算进这条判定会让两类
  400 合起来把 `via[]` 夹空。那条张力由 §5.D 的规则生成器用一条**窄放行**解掉——它**排在地板 drop 之上、
  同一条链**,即靠**次序**开洞而**不收窄地板**(§2.4 那段边界;形状与逐条反例见 network.md D5 ⑩),不由这
  两类 400 承担。
  **压第二跳所用的 uid 是部署期交给引擎的输入,不是声明字段**(§5.D)。

  **另有一条网络 400 不属于上面那张清单:它是部署的性质,不是声明文本的性质** —— **一份带 `via[]` 的声明
  遇上未给第二跳 uid 的部署,拒(400)**。上面每一类都能只读声明文本判定,而这一条即使声明**逐字合法**也要拒。
  **为何必须拒**:第二跳靠一张按部署期 uid 收窄的 `output` 表压住(§2.4 / network.md D5 ㉔),没有 uid 就没有
  那张表、也就**没有 §2.4 第二条 invariant 的强制**;收下它等于落库一份引擎兑现不了自己 invariant 的声明,
  宿主上留着一个未被压住的 confused deputy,而**第一跳的每条断言对它完全不敏感**(实测:第二跳在 `forward`
  上计 0,第一跳那份地板对它一个包都不评估)。判据与上面那张清单同形——不读别的行即可判定、无重试能让它合法、
  且没有任何东西满了故 507 是谎——但**它必须与上面各类可区分**,因为**remedy 不同:这一条的 remedy 是运维的
  (给引擎起步入参补上第二跳 uid),不是调用方的**(改声明改不动它)。故它自带 sentinel。
  它**不并入 `via[].endpoint` 非宿主本地那一类**:后者判的是 endpoint 地址,这一条与 endpoint 取值无关。

  **同一个运维条件还有一个 placement 侧的孪生,而它是 409 不是 400**:一份**已落库**的 `via[]` 声明,在
  **resume 或 publish** 上遇到未给第二跳 uid 的部署 → **409**。差别不在判据(与上面那条逐字相同),在
  **调用方这次发了什么**:resume 与 publish **不携带声明**,故 400 会让调用方去改一个他这次没发的东西。
  两条记录在案的 remedy 都改**别处**的状态(带 uid 重启引擎,或 `PATCH` 掉那个 `via[]`),这正是 409
  ——"状态冲突、可重试"——的含义:落库的记录与在跑的部署冲突。**不许压成 500**:500 说的是"引擎故障、请重试",
  而这里什么都没坏、重试也永远不会好。

  **第七类只覆盖 `egress.allow[]`,且只认地板里 loopback 与 link-local 两项**(`127.0.0.0/8` · `::1/128` ·
  `169.254.0.0/16` · `fe80::/10`)。判据是**整段包含**而非相交:地板压在声明之上是契约(见下面次序两条),
  故一条整段落在这些项内的 `allow[]` 是**永不被求值的死规则**——实测它自己的计数器恒 0 而地板计数在涨
  (network.md D5 ㉑)。收下它的代价不是"多一条无害规则":它在**"计数器为 0"这个唯一的地板健康信号上制造
  第二群合法的 0**,而每条 deny 验收都靠读这个信号。故与"无帽沙箱不可加 disk 帽"同判据——**可证明
  兑现不了的声明在写点当场拒**。**`port` / `proto` 不参与判定**:地板 daddr 项不带端口限定,按地址被包含的
  条目在任何端口上都死。**这一类刻意不覆盖三样**:
  ① **sibling CIDR 内的条目**——生成器把声明的放行**同时发在 `input` 与 `forward`**(network.md D4.2),故一条
  落在 sibling CIDR 内的 `allow[]` **不是死规则**:本沙箱自己的 `/30` 网关就在那个 CIDR 内,而它正是 §5.D
  要求调用方自己声明的那个宿主本地解析器地址(唯一稳定可用的一个,D5 ⑦)——即**这是被批准的声明路径,不是
  漏网**。到 sibling **自己**网关的那一半由 `input` 侧一条 sibling drop 兜住(同样在 D4.2),那是规则次序的
  职责、不是这一类 400 的。**附带一条独立理由**:一个提到 sibling CIDR 的 400 会把沙箱地址空间摆到 wire 上,
  那是本节 ID 寻址明确排除的。② **命名宿主本地地址的条目**——判定它要枚举宿主地址,而地板的"宿主自身的
  任一地址"是**一条纯 ct-state 规则、从不被算出**(§2.4 / D5 ⑧);枚举它既把 D8 已收口的那类问题请回来,又不
  满足"无重试能让它合法"(宿主事后多一个地址就翻转),还会让**合法的内网超网不可声明**。③ **部分重叠**
  ——`10.0.0.0/8` 与落在地板内的沙箱 `/16` 必然相交,而实测次序把它解对(D5 ㉑),故判据**必须是整段包含**。
  **`via[].match[]` 一律不判这一类**:实测被引流的流量经 `prerouting` DNAT **先于** filter 地板,落进
  `input` 后由 D5 ⑩ 那条窄放行接住——即**地板内目标的 `via[]` 是活的**(D5 ㉑)。把这一类推广到
  `via[].match[]` 会杀掉"把地板内目标交给引擎外进程判"这个用法,而那正是 `via[]` 的主用例之一。
  **两个字段在此处判定不对称是机制事实,不是遗漏。**

  **第八类是第七类的必要前置,不是独立的洁癖。** 实测 `netip.Prefix.Contains` **不**做 v4-mapped 解包
  (`169.254.0.0/16` 对 `::ffff:169.254.169.254` 返回 false,只有 `.Unmap()` 之后才 true),故一个按家族
  分支的包含判定会把 `::ffff:169.254.169.254/128` 判成 v6、只与 v6 成员比、**放它过**——而该包以 IPv4 离开、
  死在 v4 地板上(D5 ⑲)。**API 收下而线上静默拒绝**,正是 D4.3 那类诊断错位。故在**任何包含判定之前**就拒掉
  这类字面量,而不是"归一化后再比":§5.D 说声明只收 IP 字面量,`::ffff:192.168.1.1` 是一个已有地址的**第二种
  拼法**,收下它意味着落库的 Binding 与调用方的文本不同,而两种拼法在后续每一次 read-modify-write 里都比不
  相等。**同时**在谓词内部照样 `.Unmap()`,这样将来若有人撤掉这条拒绝,包含判定不会静默地变成家族盲。
  **publish 绑 loopback 不在其列**——那不是一个不合法的请求,而是一个合法请求下 guest 侧的连接被拒(§5.D)。
  **`ingress.publish[]` 的 `proto: udp` 合法,不 400**:`via[]` 排除 UDP 是因为取不回原始目标
  (network.md D3.2),而一条 DNAT 到固定 guest 端口的入站规则不需要那个机制,故那条反对不适用。
  **一条流同时命中 `egress.allow[]` 与某条 `via[].match[]` 时,`via[]` 胜**——两者是可观测不同的两个结局
  (引流 vs 直放),而一份既放行又引流同一条流的声明,最合理的读法是"把它引走";若真要直放,把它从
  `via[].match[]` 里排除掉,而不是靠加一条 `allow[]` 覆盖。**多条 `via[]` 相互重叠时按声明中的先后取
  first-match-wins**,后面的不再求值。规则顺序是安全性质而非风格(network.md D1 ④),故这两条次序必须在
  契约里定死、不留给实现:同一份声明在两个实现下必须产生同一个可达集。
  **校验在准入门之前**:一份不合法的声明在动任何字节 / 记任何预算之前就被拒,故 400 与 507 不会同时成立
  ——一次请求要么因声明不合法被 400,要么声明合法而节点装不下被 507。
  `size` 在被计量的 disk 轴上缺失(create / snapshot / clone 三处)→ **400**;卷帽降到低于当前用量
  → **400**;挂载绑定内部自相矛盾(同一沙箱内 dest 重复 / 同卷重复 / 卷间嵌套)→ **400**(§5.C:
  不读别的行即可判定,且无重试能让它合法);后端不支持 snapshot/clone → **400**(capability 查询的结果,
  不静默,且必须在动字节之前问)。
  **`GET /v1/events`**:正常(含空窗口、空页)→ **200**;`after` 或 `limit` 不是整数 / `after` 为负 /
  `limit` ≤ 0 → **400**(`limit` **超**上限不是错误,截到上限后仍 200);非 GET → **405**;
  **410 = `after` 早于保留窗口**:**保留语义**,今日不可达、不实现(理由见 §6.1)。
- **store / runtime provider 走接口**:换后端 / 加 provider 是注册一个新实现,不改上层模型
  (§1.3 只做加法;provider 始终是抽象契约,见 §1.1 / §5.A)。
