# CPA 定向预热

将按凭证、按模型族的定时预热放在 CPA 原生插件内运行。插件独立于额度保护，不修改凭证的 `prefix`、`disabled`、`excluded_models` 或保护所有权。作者：Harold Labadie；MIT；当前版本：0.1.0。

## 兼容与依赖

- Linux amd64、glibc ≥2.35、CPA ABI 1 / RPC schema 6。使用 CPA v8.0.33 的接口；生产镜像与补丁需单独做兼容验证。
- CPAMP v1.14.7 的持久事件查询和管理代理是首版必需依赖。仅有原生 usage 内存队列不能证明历史完整，不能替代 CPAMP。
- 普通公开 `/v1/chat/completions`、专用 API Key、每凭证事先配置且唯一的 prefix。Key 模型权限及全局 deny 仍由 CPA 与已有权限插件执行。
- 不需要安装 `cpa-quota-guard`。两插件各有自己的目录、锁与状态，不共享可变文件。

## 安装

1. 先核对 Release 附件 `checksums.txt` 中的 zip SHA256，再解压发布包并核对包内 `BINARY_SHA256SUMS`，将 `.so` 放入 CPA 的插件目录。不要覆盖其他插件。
2. 建立独立 0700 数据目录、0600 `secrets.json`，分别填 CPAMP panel 管理密钥与专用预热 Key。两者不是同一种密钥；不要将秘密写入插件 YAML。
3. 参照 `config.example.yaml` 配置宿主插件。CPA 地址必须是容器内回环地址，CPAMP 使用同一私有网络的地址；容器内不能照搬宿主映射端口。
4. 首次用 `observe: true` 和独立临时数据目录验证清单、额度、事件与管理页。观察模式不发送生成请求。已有旧守护时先按下文迁移，不能并行运行两个调度器。
5. 配置每个账号的唯一 prefix。缺失、重复或非法 prefix 时只等待；插件不会自动修正路由。

`enabled: false` 是宿主不加载插件。需要保留界面并暂停动作时使用管理页开关，或宿主 `observe: true`。卸载前先 quiesce，等待发送线程退出，再改宿主配置。

## 管理页面

CPA 插件菜单中的“定向预热”提供自动预热开关、上海时区每天的小时、三个模型族的模型、全部账号或指定账号。默认每天 05/10/15/20 点，90 分钟执行期限，旧周期截止后 15 秒缓冲。空账号列表代表全部支持的 Codex / Antigravity 凭证。调整计划只影响未来可执行槽位，已经发送的记录保持不变。

页面是公开静态 shell，账号和状态经受宿主管理鉴权的接口读取；管理令牌只留在当前页面内存。状态区区分等待、未决、请求完成、周期确认与激活类型。没有清空历史或强制重发按钮。所有用户数据通过文本节点显示。

同源 CPAMC 管理会话存在时，页面只读现有 `cli-proxy-auth` 会话并在内存复用；不写入浏览器存储。没有会话或会话失效时才提供手动管理令牌输入。

| 接口 | 作用 |
| --- | --- |
| `GET /v0/management/plugins/cpa-warmup/status` | 版本、工作线程、错误、下次计划与槽位 |
| `GET /v0/management/plugins/cpa-warmup/settings` | 业务设置与 ETag |
| `PUT /v0/management/plugins/cpa-warmup/settings` | 严格 JSON；必须带当前 `If-Match`，旧页面返回 412 |
| `GET /v0/resource/plugins/cpa-warmup/settings` | 不含账号和秘密的静态页面 |

设置 ETag 来自内容 SHA256，热重载或重启不会把版本归零。页面出错保留用户输入，不擅自覆盖新配置。bootstrap 地址与秘密路径不能由业务页面修改。

首版每个 tick 自行采样所有列入范围的账号，默认 20 秒；不复用另一个插件的缓存。因此未受保护池的采样可能比旧守护的 60 秒更频繁，可以通过 `check_seconds`（5–300 秒）统一调整。额度超过 90 秒不用于调度，真实零用量准入前活动历史仍重新完整查询。

## 证据与失败语义

发送前先持久化 UUID、槽位、账号身份摘要、目标 AuthID/index 和模型。全部请求 `stream=true`、`reasoning_effort=low`、`max_tokens=1`；45 秒超时。SSE/HTTP 200 本身不证明成功。before-auth 绑定可信 caller scope / TraceID / RequestID，after-auth 核实真实 selected AuthID/index；缺失、错投或 journal 写失败会正常返回 `Terminate=true`，不会返回被宿主忽略的 error。

每次发送前先经 CPAMP 管理代理回读本插件 `/status`，核对当前实例随机 nonce 与工作线程状态。CPA 从 active capability snapshot 构建管理路由；新动态库的旧路由固定旧 RPC client，因此旧实例不能冒充新 nonce。相同动态库重配复用 client，而本插件始终注册相同 before/after 能力，旧 snapshot 也会调用新的内存门控实例。该精确宿主契约变化时需重新验收；状态路由未发布、旧 nonce、错误或暂停均只等待，不用真实生成探测就绪。

持久事件必须精确匹配 Key hash、`cpa-quota-guard/2 <UUID>`、凭证文件与 index、provider、请求模型、low、时间、request ID 与已有 trace。查询采用固定时间界限、双游标、完整总数和 coverage 校验；失败、截断、迟到插入造成总数变化均为 unknown。CPAMP v1.14.7 的 scope projection 会漏 UA；attempt 使用经源码和隔离测试验证的 `UUID%` 搜索强制 raw 路径，再本地 exact 校验。版本变化后须重做契约验证。活动查询合并最近开始事件和历史超长请求，按请求开始加非负耗时求完成时间。

真实周期需请求后的新鲜 quota，非零使用量或至少相隔 10 秒、截止差 ≤3 秒的稳定满额样本。移动的满额截止提示不证明激活。Codex 精确请求成功后 180 秒仍没有周期，可记“请求完成，周期未核实”，不会重发同槽。未知、499、5xx、取消、断连、无事件均不重发；只有匹配的明确服务端拒绝才有限重试（最多 3 次，至少 60 秒），403 策略拒绝停止该槽。历史 Codex 未决只有完整旧窗口及新鲜后续真实周期证据才能退休。

只观察 CPA 的持久历史，不能证明绕过 CPA 在官方客户端的使用。Antigravity 的非流 credits fallback 可能绕过 after-auth，因此预热严格使用流式路径；同 auth 的 401 刷新重试不一定再次执行 hook。

状态绑定 name/provider/index/email/account_id/project_id 摘要。同名换账号或迁移身份无法证明时保留旧记录并停止该账号新发送；不能用清空状态绕过未决。

## 持久化、升级与迁移

数据目录保存 `state.json`、`worker.lock`、`settings.json`。0600 临时写入、fsync、rename 和目录 fsync；单实例 flock。损坏、未知 schema、锁竞争或写失败均停止动作。至少保存七天槽位；sending/sent/unconfirmed 不按年龄删除。16 MiB 状态容量到达时停止发送，不能丢未决证据。

持久 `worker.lock` 同时标记目录已经初始化。已经存在该标记却丢失 `state.json` 时拒绝重新初始化；删除状态不能恢复发送。应从有效最新备份恢复并核对未决。

schema 1 结构为 `plugin:"cpa-warmup"`、`migration`、`legacy`、`latest_success`、`attempt_gates`、`identities`。`legacy` 无损保存完整旧 v2 对象与未知字段，槽记录保留 UUID/request_id/trace/event_id/状态；新字段增加 event_hash、目标身份等。`attempt_gates` 独立保存门控证据，不改旧槽语义。

离线迁移工具由运维仓库提供，默认 dry-run，显式 apply 才写入且拒绝已有目标；停止旧调度后才导入最终状态。身份摘要必须由随包 helper 计算：

```sh
printf '%s' '{"name":"example.json","provider":"codex","auth_index":"example-index","email":"user@example.invalid","account_id":"example-account","project_id":""}' | ./cpa-warmup-helper identity
./cpa-warmup-helper validate-store /path/to/staging/state.json
```

helper 身份输出为 `{"identity":"<sha256>"}`；只读校验命令不取得 worker 锁，也不写入状态。生产切换还必须核对来源稳定性、旧未决数量与身份证明。无法证明的旧账号用阻断标记保留，不能擅自把当前同名凭证认作原账号。

回滚先 quiesce 并确认无发送在途，将**最新** `legacy` 槽与未决导出回旧 v2，而不是恢复迁移前快照。新 attempt 沿用旧 UA 与持久事件格式。额度保护所有权由它自己的迁移工具校验。无法无损回灌时保持 warmup 关闭，保留保护与状态，不冒险重发。

## 构建与验证

```sh
go test ./...
go test -race ./...
go vet ./...
node --check web/settings.js
node --test web/settings.test.js
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o cpa-warmup.so .
CGO_ENABLED=1 go build -trimpath -o cpa-warmup-helper .
```

CI 使用 Ubuntu 22.04 构建兼容 glibc 的 Linux amd64 产物，上传 `cpa-warmup_0.1.0_linux_amd64.zip` 与 `checksums.txt`，不自动创建 GitHub Release。隔离验收必须使用部署镜像、假凭证、假 Key、假上游与 CPAMP event adapter；不能耗尽真实额度或写生产监控库。测试时可以把业务槽位设为当前上海小时来驱动真实 worker，无隐藏生成接口。

ABI 声明与管理 envelope 参考 `key-model-access` 的 MIT 实现；来源与许可证见 `THIRD_PARTY_NOTICES.md`。本插件不修改或重新发布现有权限插件。
