# GuluDrive

根据 Google Photos 安卓端协议实现的网盘工具
提供独立的 Web 与 MyGo 原生界面

## 特性
    - 模拟Pixel XL无限容量
    - 支持伪装BMP/MP4来实现任意文件上传
    - 使用Sqlite3来实现文件系统映射
## 启动

| 程序 | 界面 |
| --- | --- |
| `guludrive-平台-架构` | Web 服务与原生窗口 |
| `guludrive-no-ui-平台-架构` | 只有 Web 服务 |

默认 **http://127.0.0.1:8787/**，API 为 `/api`，WebDAV 为 `/dav`

## 上传

小文件始终使用单个 BMP，大文件在设置中选择载体：

| 大文件方式 | 默认分段 | 可设范围 | 单个完整载体上限 |
| --- | --- | --- | --- |
| **BMP（默认）** | 150 MB | 100–150 MB | 150 MB |
| MP4 | 6 GB | 1–9 GB | 9 GB |

**MP4 不支持断点续传**

## 下载

- **本地**：经 GuluDrive 转发下载流
- **GDirect**：改用验证过的 Google IP 转发
- **CF Proxy**：经 Cloudflare Worker 直连 Google，文件流不经本机

链接默认 15 分钟有效

## 构建

需要 Go **1.27.3**；构建 Web 资源另需 Node 22+。首次检出先取字体（MiSans 许可禁止单独分发字体，仓库只保留声明与许可原文）：

```sh
pwsh -File scripts/fetch-fonts.ps1     # Windows
sh scripts/fetch-fonts.sh              # Linux / macOS
```

```sh
make web                     # 构建内嵌 Web 资源（唯一需要 Node 的步骤）
make all                     # Web 资源 + 本机两版
make check                   # go vet / go test / Worker 测试

make linux-arm64             # 跨平台：仅 Go 构建，需先 make web
make windows-amd64 no-ui-windows-amd64
make matrix                  # 一次编译全部 12 个产物
```

产物位于 `dist/guludrive[-no-ui]-平台-架构[.exe]`，平台为 `windows linux darwin`，架构为 `amd64 arm64`

**推 `v*` 标签时 GitHub Actions 并发编译全部 12 个产物并发布 Release**

## 许可

字体为 **MiSans 4.009**（小米），未做修改，声明与许可原文见 `web/public/legal/`，运行时可从 `/legal/` 读取。MiSans 许可禁止改造字体或单独分发字体文件，因此字体文件不入库，请用上面脚本获取。

Vue、Vite、Lucide 及其余依赖各自遵循其许可。

## 参考

- 谷歌相册协议：[gotohp](https://github.com/xob0t/gotohp)、[gpmc](https://github.com/xob0t/gpmc)
- 谷歌优选：[gdirect](https://github.com/mizorewww/gdirect)
- 原生UI实现：[mygo](https://github.com/egoist/mygo)
