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
//   - 唯独上传改走 App 专用高速节点 hyupload.pan.wo.cn，不再使用 SDK 里
//     被限速的 tjupload.pan.wo.cn / GetZoneInfo 网页节点；
//   - 用户提供的 access token 对两条通道通用（见 ltpan.py 的实测）。
type Addition struct {
	driver.RootID

	RefreshToken string `json:"refresh_token" help:"RefreshToken 或 AccessToken 二选一；AccessToken 至少 16 位（AES key 取前 16 位）"`
	AccessToken  string `json:"access_token" help:"可选；缺失或长度不足时自动用 RefreshToken 刷新"`
	FamilyID     string `json:"family_id" help:"家庭空间 ID，留空使用个人空间"`
	SortRule     string `json:"sort_rule" type:"select" options:"name_asc,name_desc,time_asc,time_desc,size_asc,size_desc" default:"name_asc" help:"目录排序规则"`

	UploadHost   string `json:"upload_host" default:"https://hyupload.pan.wo.cn" help:"App 高速上传节点（不限速）；请勿改成网页版节点"`
	TransferHost string `json:"transfer_host" default:"https://b.smartont.net" help:"秒传 / 文件校验节点；仅在开启秒传时使用"`
	ChunkSize    int64  `json:"chunk_size" type:"number" default:"16" help:"分片大小（MB），默认 16，允许 8-128；小于 8 会被自动提升（服务端要求分片不小于 8MB）"`
	Concurrency  int    `json:"concurrency" type:"number" default:"4" help:"分片并发上传数，默认 4，允许 1-16"`
	// 默认关闭：秒传要先完整扫描文件算 SHA256 再访问 b.smartont.net，
	// 该节点在部分服务器网络下响应极慢甚至不可达，会把上传拖到「几十 K/s」。
	// 离线下载 / URL 转存的文件几乎不可能秒传命中，属于纯亏的前置开销。
	EnableQuick bool `json:"enable_quick" type:"bool" default:"false" help:"上传前尝试秒传（需计算整文件 SHA256）；默认关闭，离线下载场景建议保持关闭"`
	RetryTimes  int  `json:"retry_times" type:"number" default:"2" help:"单个分片失败后的重试次数"`
}

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
