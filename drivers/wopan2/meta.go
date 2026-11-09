package wopan2

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// Addition 是「联通网盘2」驱动的可配置项。
//
// 设计说明：
//   - 目录浏览、下载链接、新建/删除/重命名/移动/复制、容量查询等仍复用
//     wopan-sdk-go 的 dispatcher（网页网关 s.pan.wo.cn 业务通道），稳定可靠；
//   - 上传走 App 通道：tjupload.pan.wo.cn + App headers + 并发分片，
//     API 与 ltpan.py 完全一致（upload2C）。实测 ltpan.py 默认的
//     hyupload.pan.wo.cn（也是 SDK GetZoneInfo 的返回值）已被限速到约
//     0.3MB/s，改用 tjupload 后可达 5~7MB/s；
//   - 用户提供的 access token 对两条通道通用（见 ltpan.py 的实测）。
type Addition struct {
	driver.RootID

	RefreshToken string `json:"refresh_token" help:"RefreshToken 或 AccessToken 二选一；AccessToken 至少 16 位（AES key 取前 16 位）"`
	AccessToken  string `json:"access_token" help:"可选；缺失或长度不足时自动用 RefreshToken 刷新"`
	FamilyID     string `json:"family_id" help:"家庭空间 ID，留空使用个人空间"`
	SortRule     string `json:"sort_rule" type:"select" options:"name_asc,name_desc,time_asc,time_desc,size_asc,size_desc" default:"name_asc" help:"目录排序规则"`

	UploadHost   string `json:"upload_host" default:"https://tjupload.pan.wo.cn" help:"上传节点，默认 tjupload（实测 5~7MB/s）。重要：管理后台已保存的旧配置不会随代码更新，必须手动改成 https://tjupload.pan.wo.cn！候选：tjupload（推荐）、hyupload（ltpan.py 默认，已限速约 0.3MB/s）、gxupload:8443（已停用）。可用随附的 diagnose.py 在你的服务器上测出最快节点"`
	TransferHost string `json:"transfer_host" default:"https://b.smartont.net" help:"秒传 / 文件校验节点；仅在开启秒传时使用"`
	ChunkSize    int64  `json:"chunk_size" type:"number" default:"8" help:"分片大小（MB），默认 8，允许 8-128。注意：管理后台已保存的旧配置不会随代码默认值变化，需手动改"`
	Concurrency  int    `json:"concurrency" type:"number" default:"4" help:"分片并发上传数，默认 4，允许 1-16。若节点/账号被服务端限速，并发不会提速，建议设为 1"`
	// 默认关闭：秒传要先完整扫描文件算 SHA256 再访问 b.smartont.net，
	// 该节点在部分服务器网络下响应极慢甚至不可达，属于纯前置开销；
	// 离线下载 / URL 转存的文件几乎不可能秒传命中。
	EnableQuick bool `json:"enable_quick" type:"bool" default:"false" help:"上传前尝试秒传（需计算整文件 SHA256）；默认关闭，离线下载场景建议保持关闭"`
	RetryTimes  int  `json:"retry_times" type:"number" default:"2" help:"单个分片失败后的重试次数；限速严重时重试会进一步拉低平均速度，可设为 0"`
}

// 注意（实测结论）：hyupload.pan.wo.cn 这个节点被服务端限速到约 0.3MB/s，
// 与账号、token、分片大小、并发数、秒传开关均无关；Python 原程序、SDK
// 网页通道、本驱动只要打到该节点，速度完全一致。tjupload.pan.wo.cn 走
// 同一个 upload2C 接口可达 5~7MB/s。排查慢上传时先看日志里的
// 「生效配置: 节点=...」和「实测 X MB/s」，再决定改哪个节点。

var config = driver.Config{
	// 前端缺少对应 i18n 词条时会直接显示 Name 本身，因此用中文名最直观。
	Name:              "联通网盘2",
	DefaultRoot:       "0",
	NoOverwriteUpload: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Wopan2{}
	})
}
