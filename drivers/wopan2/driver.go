package wopan2

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/wopan-sdk-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

// minAccessTokenLen 与 SDK 内部校验一致：access token 的前 16 字节被用作
// dispatcher 请求体的 AES key，短于 16 位必须先刷新。
const minAccessTokenLen = 16

// Wopan2 = 联通网盘2。
//
// client 负责所有 dispatcher 业务（复用 wopan-sdk-go）；
// uploadClient 是独立的、面向 App 高速节点的 HTTP 客户端，
// 与网页 dispatcher 的 UA / 超时配置完全隔离。
type Wopan2 struct {
	model.Storage
	Addition
	client          *wopan.WoClient
	uploadClient    *resty.Client
	defaultFamilyID string
	deviceID        string
}

func (d *Wopan2) onTokenRefreshed(accessToken, refreshToken string) {
	d.setTokens(accessToken, refreshToken)
	op.MustSaveDriverStorage(d)
}

func (d *Wopan2) setTokens(accessToken, refreshToken string) {
	d.AccessToken = accessToken
	d.RefreshToken = refreshToken
}

func (d *Wopan2) Config() driver.Config {
	return config
}

func (d *Wopan2) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Wopan2) Init(ctx context.Context) error {
	d.deviceID = randString(deviceIDLen, deviceCharset)
	d.uploadClient = newUploadClient()
	// 打印「数据库里实际保存」的配置，便于确认旧配置是否被覆盖。
	// 代码里改默认值不会影响已保存的存储配置，这一点最容易造成误判。
	log.Infof("[联通网盘2] 存储生效配置 | 节点=%s 分片=%dMB 并发=%d 秒传=%v 重试=%d（如需调整请在管理后台编辑该存储）",
		d.uploadHost(), d.partSize()>>20, d.concurrency(), d.EnableQuick, d.retryTimes())

	d.client = wopan.DefaultWithRefreshToken(d.RefreshToken)
	d.client.SetAccessToken(d.AccessToken)
	d.client.OnRefreshToken(d.onTokenRefreshed)
	return d.initClient(ctx)
}

// initClient 与原有 WoPan 驱动保持一致的初始化顺序：
//  1. 给 dispatcher 的底层 resty 客户端设置浏览器 UA（否则边缘节点返回 403）；
//  2. 缺失 / 过短的 access token 先刷新（否则 AES key 为空导致加密失败）；
//  3. InitData 准备用户数据；
//  4. 尝试查询默认家庭空间（仅家庭空间需要，个人空间下失败可忽略）。
func (d *Wopan2) initClient(ctx context.Context) error {
	d.client.SetUserAgent(wopan.DefaultUA)

	if len(d.AccessToken) < minAccessTokenLen {
		if d.RefreshToken == "" {
			return fmt.Errorf("需要一个至少 %d 位的 access token 或一个 refresh token", minAccessTokenLen)
		}
		if err := d.client.RefreshToken(); err != nil {
			return fmt.Errorf("刷新 access token 失败: %w", err)
		}
		if accessToken, _ := d.client.GetToken(); len(accessToken) < minAccessTokenLen {
			return errors.New("刷新 access token 失败: 服务端返回了无效的 token")
		}
	}

	if err := d.client.InitData(); err != nil {
		return fmt.Errorf("初始化用户数据失败: %w", err)
	}
	if fml, err := d.client.FamilyUserCurrentEncode(func(req *resty.Request) {
		req.SetContext(ctx)
	}); err == nil {
		d.defaultFamilyID = strconv.Itoa(fml.DefaultHomeId)
	} else {
		// 个人空间不需要家庭 ID，这个可选查询失败不应导致整个驱动不可用。
		log.Warnf("[联通网盘2] 查询默认家庭空间失败（个人空间可忽略）: %v", err)
	}
	return nil
}

func (d *Wopan2) Drop(ctx context.Context) error {
	// uploadClient 的 http.Transport 空闲连接会在 IdleConnTimeout(90s) 后自动回收。
	return nil
}

func (d *Wopan2) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	var res []model.Obj
	pageNum := 0
	pageSize := 100
	for {
		data, err := d.client.QueryAllFiles(d.getSpaceType(), dir.GetID(), pageNum, pageSize, d.getSortRule(), d.FamilyID, func(req *resty.Request) {
			req.SetContext(ctx)
		})
		if err != nil {
			return nil, err
		}
		objs, err := utils.SliceConvert(data.Files, fileToObj)
		if err != nil {
			return nil, err
		}
		res = append(res, objs...)
		if len(data.Files) < pageSize {
			break
		}
		pageNum++
	}
	return res, nil
}

func (d *Wopan2) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if f, ok := file.(*Object); ok {
		res, err := d.client.GetDownloadUrlV2([]string{f.FID}, func(req *resty.Request) {
			req.SetContext(ctx)
		})
		if err != nil {
			return nil, err
		}
		return &model.Link{
			URL: res.List[0].DownloadUrl,
		}, nil
	}
	return nil, fmt.Errorf("无法把文件转换为 Object")
}

func (d *Wopan2) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	familyID := d.FamilyID
	if familyID == "" {
		familyID = d.defaultFamilyID
	}
	_, err := d.client.CreateDirectory(d.getSpaceType(), parentDir.GetID(), dirName, familyID, func(req *resty.Request) {
		req.SetContext(ctx)
	})
	return err
}

func (d *Wopan2) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	dirList := make([]string, 0)
	fileList := make([]string, 0)
	if srcObj.IsDir() {
		dirList = append(dirList, srcObj.GetID())
	} else {
		fileList = append(fileList, srcObj.GetID())
	}
	return d.client.MoveFile(dirList, fileList, dstDir.GetID(),
		d.getSpaceType(), d.getSpaceType(),
		d.FamilyID, d.FamilyID, func(req *resty.Request) {
			req.SetContext(ctx)
		})
}

func (d *Wopan2) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	_type := 1
	if srcObj.IsDir() {
		_type = 0
	}
	return d.client.RenameFileOrDirectory(d.getSpaceType(), _type, srcObj.GetID(), newName, d.FamilyID, func(req *resty.Request) {
		req.SetContext(ctx)
	})
}

func (d *Wopan2) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	dirList := make([]string, 0)
	fileList := make([]string, 0)
	if srcObj.IsDir() {
		dirList = append(dirList, srcObj.GetID())
	} else {
		fileList = append(fileList, srcObj.GetID())
	}
	return d.client.CopyFile(dirList, fileList, dstDir.GetID(),
		d.getSpaceType(), d.getSpaceType(),
		d.FamilyID, d.FamilyID, func(req *resty.Request) {
			req.SetContext(ctx)
		})
}

func (d *Wopan2) Remove(ctx context.Context, obj model.Obj) error {
	dirList := make([]string, 0)
	fileList := make([]string, 0)
	if obj.IsDir() {
		dirList = append(dirList, obj.GetID())
	} else {
		fileList = append(fileList, obj.GetID())
	}
	return d.client.DeleteFile(d.getSpaceType(), dirList, fileList, func(req *resty.Request) {
		req.SetContext(ctx)
	})
}

// Put 走 App 专用高速上传通道，而不是 SDK 的 Upload2C（网页限速节点）。
func (d *Wopan2) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	return d.fastUpload(ctx, dstDir, stream, up)
}

func (d *Wopan2) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	quota, err := d.client.QueryCloudUsageInfo()
	if err != nil {
		return nil, err
	}
	total, err := strconv.ParseInt(quota.UsageInfo.ByteTotalSize, 10, 64)
	if err != nil {
		return nil, err
	}
	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: total,
			UsedSpace:  quota.UsageInfo.ByteUsedSize,
		},
	}, nil
}

var _ driver.Driver = (*Wopan2)(nil)
