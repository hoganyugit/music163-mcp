# music163-mcp

让 Claude Code 等 agent 下载网易云音乐"真正"的最高音质，并识别假无损。

An MCP server that downloads the genuinely highest-quality audio from NetEase Cloud Music and detects fake lossless / fake Hi-Res files. Pure Go, single binary, no ffmpeg.

它以 MCP 服务（stdio）的形式给 agent 用，也带一套等价的命令行：

- 搜歌，并查询每个档位实际能拿到什么文件。
- 从超清母带往下逐档下载并检测，**只有实测与标称一致才保留高档**。
  - 伪 Hi-Res 会自动换成信息量相同、体积更小的低档。
  - 疑似有损转码（假无损）的文件会保留，但做标记。
- 检测本地已有文件的真伪，返回结论、证据和频谱图。

全部用纯 Go 实现，不依赖 ffmpeg，编译出来是单个可执行文件。需要网易云账号（下载无损及以上档位需要会员）。

## 快速开始

```bash
# 1. 安装（三选一）
#    a. 从 Releases 下载对应平台的可执行文件，改名为 music163-mcp 放进 PATH（如 ~/.local/bin）
#    b. go install（需要 Go 1.26+）
go install github.com/hoganyugit/music163-mcp/cmd/music163-mcp@latest
#    c. 源码编译，产物在 bin/music163-mcp
git clone https://github.com/hoganyugit/music163-mcp && cd music163-mcp && bash build.sh

# 2. 在自己的终端里扫码登录（会生成 ~/.config/music163/mcp.toml，权限 600）
music163-mcp login

# 3. 接入 Claude Code
claude mcp add -s user music163 -- music163-mcp serve
```

其他支持 MCP 的客户端按 stdio 方式配置即可：

```json
{
  "mcpServers": {
    "music163": {
      "command": "/home/you/.local/bin/music163-mcp",
      "args": ["serve"]
    }
  }
}
```

接入后可以直接对 agent 说：

- "帮我下载 XX 的《YY》，要最高音质"
- "检查一下 ~/Music 里这首是不是假无损"

服务运行中重新登录不需要重启，新的登录凭据会在 5 秒内自动生效。

## 工具

| 工具 | 作用 |
|---|---|
| `search_songs` | 搜索。返回 ID、歌名、歌手、专辑、时长、别名，以及立体声最高档。没有版权的歌和"含AI生成"的歌会标出来 |
| `get_song_qualities` | 逐档查询能拿到的文件（不下载）：格式、码率、大小、是否只给试听、是否被悄悄降档、哪些档其实是同一个文件 |
| `download_song` | 下载并检测，存进曲库。默认 `level=auto`，超过等待时间会返回 `job_id` |
| `job_status` | 等待后台下载任务 |
| `check_audio` | 检测本地 FLAC/MP3，返回判定、证据和频谱图。只能读曲库和 `check_dirs` 里的文件 |
| `account_status` | 登录状态和会员信息，不返回登录凭据 |

`download_song` 只接受歌曲 ID 或链接，不接受关键词。直接拿关键词下载很容易下到 Live 版、伴奏或翻唱，所以要先用 `search_songs` 确认是哪个版本。

## level=auto 怎么选档

立体声档位从高到低依次是：超清母带 `jymaster` → Hi-Res `hires` → 无损 `lossless` → 极高 `exhigh`（320k MP3）→ 较高 → 标准。

菁彩声 `vivid`、杜比全景声 `dolby`、沉浸环绕声 `sky`、高清环绕声 `jyeffect` 是空间音频处理版，不是原始立体声混音，自动选档时不考虑。

选档步骤：

1. **逐档请求下载链接。**
   - 读出服务器实际给的档位（没权限时会悄悄降档），记录降档情况。
   - 只给试听片段的档位不下载。
   - 和已经下载过的档位 MD5 相同的，跳过。
2. **下载、校验、检测。** 校验接口给的 MD5，再完整解码检测，把这一档折算成"实际规格"。比如"44.1kHz 音源升频到 192kHz"，或者"有损音源，截止 16 kHz"。
3. **判断要不要继续往下：**
   - 检测为真、实测与标称一致：保留这一档，结束。
   - 低一档的实际信息量不少于当前最佳：换成体积更小的低档。比如超清母带其实是 CD 音源升频，就改存无损档。
   - 低一档信息量更少：保留当前最佳，结束。
   - 已经拿到 FLAC 就不会再降到 MP3。当前最佳是真高解析（> 48kHz）时，不会再下载无损档。
4. **处理不可用的情况。** 试听片段和损坏的文件直接弃用。所有档位都拿不到完整文件时报错，并说明原因，比如无版权或需要购买数字专辑。

也可以用 `level=jymaster` 这类写法只下指定档位。这时检测结果只作报告，不会换档。

## 检测项和可靠性

| 问题 | 方法 | 可靠性 |
|---|---|---|
| 试听片段 | 接口的 `freeTrialInfo`；解码时长和官方时长对比 | 确定 |
| 被悄悄降档 | 接口返回的档位；文件实测规格和标称对比 | 确定 |
| 格式或规格不符 | 文件头、FLAC STREAMINFO | 确定 |
| 损坏或截断 | 接口 MD5、FLAC 帧 CRC、FLAC 内置的音频 MD5 | 确定 |
| 有损转码（假无损） | 平均频谱在 11 kHz 以上的"断崖"：1 kHz 内陡降 25 dB 以上，上方没有内容 | 概率性 |
| 升频（伪 Hi-Res） | 断崖位置推算原始采样率；24.5 kHz 以上的电平和量化噪底对比 | 较可靠 |
| 位深补零（伪 24bit） | 所有采样的低位是否全为 0 | 补零能确定；加过抖动的查不出 |

用真实的网易云 MP3 做过验证：

- 128k MP3 转成的 FLAC：判为 `lossy_likely`，置信度 high（16.7 kHz 处陡降 57 dB）。
- 320k MP3 转成的 FLAC：判为 `lossy_suspect`（20.3 kHz 处陡降 56 dB）。

**局限：**

- 不少 CD 母带本身就在 20 kHz 附近做了低通，所以截止在 19.6–20.6 kHz 的只判"可疑"，不下定论。
- 不带低通的高码率有损编码，以及算法补出来的高频（类似 AI 超分），都能骗过频谱检测。"未发现造假痕迹"不等于百分之百为真。
- 老录音本身频带窄，可能被误报。判断时要结合频谱图看。

## 文件存放

- **曲库**：`~/Music/NetEase/歌手/专辑/曲号 歌名.flac`。
  - 多碟专辑用 `碟号-曲号` 作前缀。
  - 文件名里 microSD 卡（exFAT）不允许的字符会换成全角。
- **标签**：TITLE、ARTIST、ALBUM、ALBUMARTIST、TRACKNUMBER、DISCNUMBER、DATE，另外有：
  - DESCRIPTION：网易云 163 key；
  - COMMENT：检测结论，有问题时带 ⚠；
  - QUALITY_CHECK：机器可读的检测结论。
- **封面**：统一成 baseline JPEG，最长边不超过 1200 像素。Walkman 不显示渐进式 JPEG 和 WebP。
- **歌词**：同名 `.lrc`，Walkman 可以直接显示。可以在配置里打开附带翻译。
- **报告和频谱图**：放在 `~/.local/share/music163-mcp/`（`reports/`、`spectrograms/`），不混进曲库。所以整个曲库目录可以直接拷进 Walkman 的 `MUSIC` 文件夹。

## 配置文件

默认在 `~/.config/music163/mcp.toml`（遵守 `XDG_CONFIG_HOME`），由 `init`、`login` 或 `serve` 首次运行时生成。各项的说明都写在文件注释里。常用项：

```toml
[netease]
music_u = ''      # 扫码后自动写入，不要手动分享
proxy = ''        # 机器在海外时填国内代理，否则很多歌拿不到链接

[download]
timeout = '300s'  # 单次下载尝试的时长上限，超时后断点续传
concurrency = 2   # 同时下载的歌曲数

[library]
dir = '~/Music/NetEase'
mount = ''        # 曲库在 SD 卡上时填挂载点：没插卡就拒绝下载，不会写到电脑硬盘
state_dir = '~/.local/share/music163-mcp'
check_dirs = ['~/Music']   # check_audio 允许读取的目录
lyrics = true
lyrics_translation = false
cover_max_px = 1200
```

所有命令都接受 `-c 路径` 指定别的配置文件。

## 命令行

命令行的输出和 agent 看到的完全一致，方便排查问题：

```bash
music163-mcp status                      # 登录和会员状态
music163-mcp search -n 5 歌名 歌手
music163-mcp qualities 歌曲ID或链接
music163-mcp download [-level auto] [-force] 歌曲ID或链接
music163-mcp check [-png 频谱.png] [-json] 文件.flac
```

## 安全

- 登录凭据 `MUSIC_U` 只保存在权限为 600 的配置文件里。
  - 不会出现在工具返回和日志里。
  - 登录只在终端扫码完成，不经过 agent。
- `check_audio` 只读白名单目录，会解析符号链接，防止绕过。
- `download_song` 只接受网易云歌曲 ID 和链接，不下载任意 URL。
- stdio 模式下标准输出只用于 MCP 协议，日志写到 stderr，或者写到 `[log] path` 指定的目录。
- 所有网易云接口请求（搜索、详情、链接、歌词、账号）之间至少间隔 300ms。同一台机器上同时运行的 MCP 服务和命令行共用这个间隔，通过 `state_dir/api.lock` 协调。默认最多 2 个并发下载。请只用于个人用自己的会员账号。

## 曲库放在 SD 卡上

用读卡器直接写 Walkman 的 SD 卡时：

- `dir` 设成卡上的目录，比如 `/media/用户名/XXXX-XXXX/Music/NetEase`。
- `mount` 设成卡的挂载点，比如 `/media/用户名/XXXX-XXXX`。

这样设置后：

- 下载、打标签都先在电脑硬盘上完成，最后再复制到卡上，并做 fsync。
- 没插卡时下载会被拒绝，并给出明确提示。
- `account_status` 会显示卡有没有插、还剩多少空间。

挂载路径由卡的文件系统 UUID 决定，和插在哪个 USB 口无关。拔卡前请先在文件管理器里"安全弹出"。

## 在海外使用

网易云按出口 IP 判断地区。2026 年 11 月用新加坡出口 IP 和 SVIP 账号实测，华语、粤语、日本、欧美各类热门歌都能拿到完整无损，超清母带也能正常下载，暂时不需要代理。

有些歌只给低码率，或者干脆不能播，原因可能是：

- 音源本身就只有这个规格，比如电视直播的现场版只有 128k；
- 没有版权，或者已经下架；
- 地区限制。

如果同一首歌在国内能播、在这里不能播，可以在配置 `[netease] proxy` 填一个国内的 HTTP 或 SOCKS5 代理。代理只用于接口请求，音频文件仍然从 CDN 直连下载。

## 常见问题

- **全部档位都不可用**：
  - 先看搜索结果有没有标"无版权"。比如周杰伦的歌在网易云没有版权。
  - 机器出口 IP 在海外时，需要配置 `[netease] proxy`。
- **下载很慢，返回了 job_id**：用 `job_status` 等待。重复调用 `download_song` 也只会接上同一个任务。
- **想重新下载**：加 `force=true`。命令行是 `-force`。换档后旧文件会被替换掉。
- **搜索返回"没有搜到"但歌明明存在**：多半是请求太密被网易云限流了，等一会再试。

## 来源与致谢

本项目从 [XiaoMengXinX/Music163bot-Go](https://github.com/XiaoMengXinX/Music163bot-Go)（一个网易云音乐 Telegram bot，GPL-3.0）派生而来：2026 年 11 月在其 v3 分支 commit `22580fa` 的基础上，去掉了 Telegram 相关的全部代码，改成只提供 MCP 服务和命令行。

来自原项目并经修改的部分：

- `internal/netease`：网易云接口封装、扫码登录
- `internal/fetch`：CDN 直连下载、断点续传、反代重试、MD5 校验
- `internal/config`：配置解析与代理地址校验
- `internal/logging`、`internal/textx`：日志、歌曲链接解析

为本项目新写的部分：`cmd/music163-mcp`、`internal/mcpserver`（MCP 服务）、`internal/acquire`（逐档下载与选档）、`internal/audiocheck`（假无损检测与频谱图）。

同时依赖原作者的几个库：[Music163Api-Go](https://github.com/XiaoMengXinX/Music163Api-Go)、[SimpleDownloader](https://github.com/XiaoMengXinX/SimpleDownloader)、[163KeyMarker](https://github.com/XiaoMengXinX/163KeyMarker)。

## 许可证

[GPL-3.0](LICENSE)，与原项目相同。

- Copyright (C) 2021-2026 XiaoMengXinX 及 Music163bot-Go 的贡献者（原项目部分）
- Copyright (C) 2026 hoganyugit
