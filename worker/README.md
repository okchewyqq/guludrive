# GuluDrive CF Proxy

1. 在 GuluDrive 设置中输入公网源站地址与代理密码，点击复制 Worker 脚本。
2. 在 Cloudflare Workers 创建 Worker，把完整脚本粘贴进编辑器并部署。无需环境变量、Secret 绑定、KV、Durable Objects 或其他存储。
3. 将部署后的 Worker 地址填回 GuluDrive，保存相同的公网源站与密码，选择 CF Proxy。源站的 `/download-manifest` 必须能从公网访问。

脚本包含密码，应像数据库一样妥善保管；建议使用长随机密码。更改源站或密码后须重新复制并部署。Workers 兼容日期需不早于 `2024-09-02`（仓库配置已满足）。`workers.dev` 使用 HTTPS；普通公网主机名可使用 HTTP 或 HTTPS 和自定义端口，实际连通性取决于部署环境，不保证公网裸 IP 在 Cloudflare 上可达。HTTP 不保护传输中的授权清单；Google 内容连接仍强制 HTTPS/TLS。拒绝内网、回环源站与跨源清单。

Go 仅提供短时授权与 v2 签名清单，文件流为 **Google → Worker → 客户端**。密码按原始 UTF-8 用于 HMAC-SHA256，签名对象是字面 `p` 参数，不派生密钥；链接默认 15 分钟、最长 1 小时。链接和脚本均敏感，勿记录 URL 或密码日志。

所有完整及 `meta=1` 清单响应均须携带 `X-GuluDrive-Manifest-Auth`：原始 UTF-8 密码对查询 `p` + 换行 + 原始 JSON 字节的 HMAC-SHA256（无填充 base64url），Worker 验证后才解析元数据。

保留跨分段流、条件请求与 Range（最多 8 个成员 / 2048 字节）、16 MiB 清单和 120 秒元数据窗口、Google 域名及重定向白名单、取消与 `FixedLengthStream` 精确长度边界。不完整缓冲文件、不哈希、无本地回退。

离线验证（Node 22+；全部 fetch 为 mock）：

```sh
node --check worker/worker.mjs
node --check worker/worker.test.mjs
node --test worker/worker.test.mjs
```
