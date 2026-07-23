# M5 — 统一对象模型 + 引用-回收原子性(以 Template lineage + Promote 兑现)

> **交付的能力**:让「一等持久对象带可判定的回收语义」这套地基第一次落地,并用**最尖的
> lineage 场景(Template)**把它跑通——Template 成为一等行、`create-from-Template` 与 Promote
> 叠层都是**受保护的引用**、删除/GC 安全、且用户第一次能**从沙箱固化出自己的 Template**(§1.2)。
> Volume(M6)的独占租约 = 一个 refcount 挂载行,直接复用本里程碑浇的地基。
>
> 本里程碑依赖的模型修订(§3.1 身份 / §4.3 对象模型 / §5.B 叠层+sweep / api-contract 409)**已落进
> spec 作权威**(见末尾指针);本 plan 只讲 forward scope / 验收 / out-of-scope,一律引 spec 为准。

---

## 0. 一句话结论(先读这句)

**refcount 全域统一为 derived(引用行是否存在,不物化计数字段);引用-回收竞态纯靠外键 +
单写者串行闭合,无显式 CAS、无两阶段删除;Template 用「存在即状态」(不加 state 列),这逼出
一条硬约束——不可变工件必须字节先行(bytes-first);Promote 把当前 Overlay 冻结成新 Template
的只读底层,经 multi-lowerdir 叠层,content-addressed diff 仍是演进项。**

---

## 1. 范围(in scope)

1. **统一对象模型的第一次物化**:引入 `{derived refcount}` 一等持久对象模式。Template 是首个、
   也是最尖的应用(lineage)。
2. **Template 成为一等行**(`templates` 表):opaque ID 作身份、`parent` 自引用表达 lineage。
3. **引用-回收原子性**(§4.3):`sandbox→template`、`template→parent` 两类引用经**外键 RESTRICT**
   保护;删除是一次原子 `DELETE`,竞态由 FK + 单写者 store(已 pin 单连接)闭合。
4. **删除 / GC 安全**:`DELETE /v1/templates/{id}` 落地,被引用即拒(→ 409);字节回收走
   **孤儿 sweep**(复用 §4.4-A 反向对账形状),不靠 FK 删字节。
5. **内置 Template 进对象模型**:boot 期幂等对账,为 `TemplatesDir` 下每个静态目录确保一行。
6. **Promote**(§1.2 / §3.1):`POST /v1/templates {from_sandbox_id}`——把沙箱当前 Overlay
   冻结固化成新 Template 底层,经 multi-lowerdir 叠层。

## 2. 非范围(out of scope,明确划走)

- **content-addressed diff 层 / 内容 hash 去重**:身份用 opaque ID;内容摘要是**将来**的去重/
  完整性锚点(rootfs-overlay D3 演进项)。§3.1 相应修订(见末尾)。
- **异步构建 `building/ready/failed`**:一旦 Promote/构建变成长时可失败可轮询,那是**独立
  build 表**的事(E2B `env_builds` 模式),不塞进 Template 状态。本里程碑 Promote 是**同步**的,
  build 表纯 additive 留给将来。
- **Overlay 升为一等行**:其生命周期 1:1 随沙箱、refcount 恒 1(until fork),现在升是投机预留
  (§1.3)。Template lineage refcount 不依赖它。
- **`deprecated`/`enabled` 业务 flag**:与回收模型正交,将来加 boolean,不改原子删除。
- **Volume / 挂载行 / fork / clone / 网络**:各自后续里程碑。

---

## 3. 三条承重不变量(本里程碑的全部正确性都挂在这三条上)

**INV-1 —— refcount 全域 derived,引用保护靠外键 RESTRICT。** 不物化计数字段;某对象「是否可删」=
「有没有引用它的活行」。单写者 store(已 pin 单连接 + `_txlock=immediate` + `foreign_keys(on)`,
见 `store/sqlite.go`)下,`create-from-Template` 与 `DELETE template` 并发时:谁先提交谁赢——
create 先提交则 template 多一条引用行、delete 撞 RESTRICT 失败;delete 先提交则 create 撞
missing-parent FK 失败。**无需显式 CAS**,原子性从 schema 掉出来。这是 §4.3「引用-回收原子性」的
兑现,直接复用 §4.4-C 的单次原子提交契约。

> **seam 纪律**:「被引用即拒删」是 **Store 接口的契约承诺**,不是「SQLite 有 FK 这个特性」。
> SQLite 用 FK RESTRICT 兑现、Memory 用 Go map 检查兑现——与「原子提交契约」两后端并列同构
> (§4.4-C 铁律:后端差异不许漏进正确性)。RESTRICT 失败在 API 层翻成干净 **409 template in use**,
> 不让裸 SQL error 冒到 500。

**INV-2 —— 外键方向:引用向上 RESTRICT,所有权向下 CASCADE。** 两个方向要相反语义,不可混:
- **引用向上**(`sandbox.template → templates.id`、`templates.parent → templates.id`):`RESTRICT`
  ——「refcount>0 绝不可删」的声明式表达。删被引用的 template / 被叠层的父层 → DB 拒绝。
- **所有权向下**(将来 `sandbox → 它自己拥有的挂载行`):`CASCADE`——删 sandbox 顺带清其所属子行。
- **铁律**:绝不让删 template 级联进 sandbox(那是删活沙箱)。本里程碑只出现向上 RESTRICT 边。

**INV-3 —— 字节/行写入顺序,按对象种类分,是孤儿 sweep 安全的前提。** 守恒式随对象种类不同:
- **不可变工件(Template,存在即状态)**:**字节先行**。create = 写字节 → 插行;delete = 删行 →
  回收字节;sweep 只删「有字节、无活行」。**反序(先插行后写字节)= 制造「行指向不存在字节」的
  破损 ready 态,sweep 救不了**。存在即状态**没有 not-ready 逃生舱**,故 bytes-first 是强制,不是偏好。
- **状态机对象(Sandbox,有 state 列)**:允许**行先行**——`creating` 就是显式 not-ready 标记,
  reconcile 扫停滞的 `creating` 兜底(现状 lifecycle.go:209 插行先于 :214 建 overlay,合法)。

> 一句话:**有 not-ready 状态列的对象可以行先行;存在即状态的对象必须字节先行。** 这条正是
> 「Template 不加 state 列」这个决定**逼出来**的硬约束,二者耦合、不是两个独立选择。

---

## 4. 为什么这个设计不踩 E2B 的坑(引用必须是持久行)

E2B 删 template 时漏查了**只活在内存里**的运行中沙箱引用(FK 看不见),删掉了在用的 template。
**我们免疫,且免疫是可指认的属性,不是巧合**:

- `sandbox→template` 引用存在 `sandboxes.template` **持久列**里,自 `creating` 插行起,贯穿
  running/paused/destroyed 全程。**唯一内存瞬态的是 placement**(live-map:netns/IP/merged 句柄)
  ——而 placement 不是 template 引用。
- 故 paused/快照沙箱(E2B 丢的正是这个 case)仍持一条指向 template 的持久行 → FK RESTRICT 看得见
  → 删不掉。
- **要守的边界条件**:任何引用 template 的代码路径,必须在**开始使用该 template 之前(或同一事务内)**
  落定引用行。sandbox create 已满足(行先行);Promote 的 `parent` FK 同理(插子 template 行时
  带 parent FK,原子)。

---

## 5. Schema(`templates` 表 + FK 边)

```sql
CREATE TABLE IF NOT EXISTS templates (
    id             TEXT PRIMARY KEY,           -- opaque "tmpl_<token>"(内置=静态目录名)
    parent         TEXT REFERENCES templates(id) ON DELETE RESTRICT, -- lineage;NULL=base 层
    content_digest TEXT,                       -- nullable;去重/完整性的将来锚点,现在不填不用
    builtin        INTEGER NOT NULL DEFAULT 0, -- 1=平台预置(boot 对账 seed),0=Promote 出的
    created_at     INTEGER NOT NULL            -- unix nanos
);
```

`sandboxes.template` 加外键(**破坏性改 schema、直接重建,无 migration**,CLAUDE.md「无兼容负担」):

```sql
-- sandboxes 表:template 列加 REFERENCES + RESTRICT
template TEXT NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
```

- **无 `state` 列**(存在即状态,§3 INV-3)。**无物化 refcount 列**(derived,§3 INV-1)。
- **lineage = `parent` 自引用 + RESTRICT**:一张自引用表 + 一条 FK 表达「分层 Template」全部;
  overlayfs 的 lowerdir 栈 = 沿 `parent` 链走(§7)。
- **前提**:上外键前须确认 `templates` 行在 `sandboxes` 引用它**之前**已存在——boot seed(§8)
  保证内置层在任何沙箱 create 前就位;Promote 出的层在其被引用前已插行。

## 6. 三个引用-回收事务形状(标出 FK 在哪闭合竞态)

**create-from-Template**(现 `Create`,加 FK 保护):
1. `templates` 中该 id 必须存在(boot seed / 之前 Promote)。
2. 插 `sandboxes` 行(`creating`,`template=<id>`)——FK 校验父 template 在 → 若并发 delete 已删掉,
   **插入撞 missing-parent FK 失败**(create 输,正确)。
3. 其余冷启链不变(§5.E.1)。

**DELETE template**(新):
1. 单条 `DELETE FROM templates WHERE id=?`。
2. 若有 `sandboxes.template=<id>` 或 `templates.parent=<id>` 的活行 → **撞 RESTRICT 失败** →
   API 层 409 template in use(delete 输,正确)。内置层(`builtin=1`)同样受 RESTRICT 保护;
   平台可删一个无引用的内置层,有引用则一视同仁拒。
3. 成功删行后,该 template 的目录字节成孤儿 → 归孤儿 sweep(§9),**不在删路径同步删字节**
   (§4.4 状态转换与字节回收解耦)。

**Promote**(新,§3.1 / 详见 §7):在源沙箱**静止到一致点后**读冻结 upper → 写新层字节 →
插 `templates` 行(`parent=<源沙箱的 template>`,`builtin=0`)。**字节先行**(§3 INV-3):
crash 在「写完层字节、插行前」→ 孤儿层字节 → 被 sweep 回收,无破损 ready 态。

---

## 7. Promote 机制(§3.1 / §5.B)

**语义**:接受 `running` 或 `paused` 源沙箱。若 `running`,内部走
`pauseLocked` 静止到一致点(等价一次 pause),读冻结 upper,**完成后留 `paused`**(调用方按需
resume)。全程持源沙箱的**转换锁**(§4.4-B),故与并发 destroy/resume 串行。

**一致点**:D3 e2e 已证「干净停(kill+delete)后 upper 是一致的宿主普通目录树、
字节逐字节一致」。协作式 flush 是优化、非正确性前提(§4.4)。故本里程碑 Promote **读现有抢占式
停机后的 at-rest upper**;协作安全点仍延期(open-decisions §2 §4.2 行 / guest-runtime D4.4)。

**叠层机制:multi-lowerdir overlayfs。** Promote 把源沙箱冻结 upper 拷成一个**新只读层目录**,
新 Template 的 lower 栈 = `[新层, ...源 Template 的 lower 栈]`(沿 `parent` 链展开)。
content-addressed diff(提 OCI tar + digest)仍是演进项(rootfs-overlay D3),抽象不变。

**excluded paths(待定精确清单,本里程碑给最小集)**:固化时须过滤引擎自身注入的运行时路径——
**至少 `sandboxd` 的 portal/工作目录**。Volume 挂载点占位要等 Volume(M6)才出现,现在无。
精确清单登记 rootfs-overlay D5(已有该条),本里程碑落最小集 + 测试兜住 sandboxd 路径不进层。

## 8. 内置 Template 进对象模型(boot seed)

boot 期一次**幂等对账**:枚举 `TemplatesDir` 下每个静态目录,确保 `templates` 有对应行
(`builtin=1`,`parent=NULL` 或按目录约定的 lineage)。**字节先行**天然满足(目录随二进制/部署已在),
只补插行。与 §4.4-A startup reconcile 同一时机、同一 skip-and-log 失败策略。**必须在开始服务
(接受 create)之前完成**,否则 `sandboxes.template` 的 FK 会撞不存在的父行。

## 9. 孤儿字节 sweep + in-flight 构建守卫(本里程碑引入的新竞态)

**孤儿 sweep**:磁盘上没有对应活 `templates` 行的层目录 = 孤儿 → 回收。复用 §4.4-A 反向对账形状
(权威 store × 本机资源 diff),不新造 reclaim 队列。lineage 下「层目录被引用」= 有 template 行以它
为自身或经 `parent` 链为祖先——FK RESTRICT 已保证删父层前子层不存在,故 sweep 只需按「有无活行」判。

**in-flight Promote vs sweep 竞态(bytes-first 引入的新窗口)**:同步 Promote 期间,层目录已在盘上、
行还没插——并发周期 sweep 看「有字节无行」会**把在建层拆在 Promote 脚下**。这是 §4.4-A placement
问题的同形:解法 = **一个内存 `buildingLayers` keep-set,sweep 尊重它**(template-layer 版的
`liveIDs()`):
- boot 时**空**(崩掉的 Promote 的 staging 层确是真孤儿 → 正确被回收);
- in-flight Promote 期间登记其层目录,周期 sweep 跳过;
- Promote 成功插行 / 失败清理后移除。

**这条现在就要设计进 sweep,别做成 lock-naive 再回填。** 是「保护在建资源不被反向 diff 拆掉」
的同一模式,也是为何周期 sweep(非仅 boot)必须知道 in-flight 构建。

---

## 10. 代码面(改哪些,按子系统)

| 位置 | 改动 |
| --- | --- |
| `internal/model` | 加 `Template` 类型(ID / Parent / ContentDigest / Builtin / CreatedAt);`TemplateID` 已存在 |
| `internal/store`(接口) | 加 Template CRUD:`CreateTemplate` / `GetTemplate` / `ListTemplates` / `DeleteTemplate`。**契约承诺**「删被引用的 template → `ErrConflict`/`ErrInUse`」(seam,两后端各自兑现) |
| `internal/store/sqlite.go` | 加 `templates` 表 + `sandboxes.template` FK RESTRICT(**破坏性改 schema、重建,无 migration**);DELETE 撞 FK → 翻成 `ErrInUse` |
| `internal/store/memory.go` | Memory 后端用 Go 检查兑现同一契约(扫 sandbox/template 引用) |
| `internal/rootfs`(Provider) | 加 `PromoteToTemplate(overlay, newTemplateID)`(读冻结 upper → 写新只读层)+ multi-lowerdir 展开 lineage;`buildingLayers` keep-set + 孤儿层 sweep |
| `internal/lifecycle` | `Promote` 动词(持转换锁 → `pauseLocked` → 固化 → 留 paused);boot seed 内置 template;周期孤儿层 sweep 接入 reaper tick(与 placement 对账并列) |
| `internal/orchestrator/api.go` | `GET/POST/DELETE /v1/templates`;DELETE 被引用 → 409 |

## 11. 验收(e2e,每条 assert 内联其 property)

1. **删被引用即拒**:create-from-T → `DELETE T` 得 409;destroy 沙箱后 `DELETE T` 成功。
2. **lineage 保护**:Promote 出子层 T2(parent=T1)→ `DELETE T1` 得 409;删 T2 后可删 T1。
3. **引用-回收原子性(并发)**:create-from-T 与 `DELETE T` 真并发压测 → 恰一方成功、另一方干净
   失败;**绝不出现悬空引用 / 破损 ready 态 / 丢字节**。
4. **E2B 坑回归**:paused 沙箱的 template 因持久行受 RESTRICT → 删不掉。
5. **bytes-first 崩溃**:Promote 写完层字节、插行前崩 → 重启 sweep 回收孤儿层,无破损行。
6. **in-flight 守卫**:Promote 进行中跑周期 sweep → 在建层不被拆(buildingLayers)。
7. **Promote 端到端**:沙箱装个包 → Promote → 新沙箱 from 新 template → 看得见那个包(固化生效);
   sandboxd portal 路径不进固化层(excluded)。
8. **boot seed 幂等**:重启两次,内置 template 行数不变、无重复。

## 12. 分期(单里程碑,内部两道门)

- **P1 — 对象模型 + 安全删除/GC(静态 template 上)**:§5 schema + §6 create/delete 事务 + §8 boot
  seed + §9 孤儿 sweep。验收 1/3/4/5/8。**门:go build+vet+这些 e2e 全绿于 `sbx`。**
- **P2 — Promote**:§7 固化 + multi-lowerdir + §9 buildingLayers。验收 2/6/7。**门:同上全绿。**
- 每道门过后再进下一段(CLAUDE.md 一次一里程碑 / 失败两次即 root-cause)。

---

## 附. spec 权威指针

本里程碑所依赖的模型已**沉淀进 spec 作权威**(不寄存于本 plan):§3.1 Template 身份(opaque ID)、
§4.3 统一对象模型(derived refcount + 存在即状态 + bytes-first)、§5.B(multi-lowerdir 叠层 +
孤儿层 sweep + in-flight 守卫)、api-contract §6.1(`DELETE` 被引用 → 409)。本 plan 只讲交付到哪、
验收怎么算,一律引 spec 为准。

> 收尾(shipped 时):open-decisions §4「实现缺口」与被本里程碑收口的 §2 条目已迁出;M5 plan 随
> shipped 而 expire,梯形 README 留一行。
