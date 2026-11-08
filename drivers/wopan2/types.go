package wopan2

import (
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/wopan-sdk-go"
)

// Object 在通用对象上额外保存文件 FID，供下载链接接口使用。
type Object struct {
	model.ObjThumb
	FID string
}

func fileToObj(file wopan.File) (model.Obj, error) {
	t, err := getTime(file.CreateTime)
	if err != nil {
		return nil, err
	}
	return &Object{
		ObjThumb: model.ObjThumb{
			Object: model.Object{
				ID:       file.Id,
				Name:     file.Name,
				Size:     file.Size,
				Modified: t,
				IsFolder: file.Type == 0,
			},
			Thumbnail: model.Thumbnail{
				Thumbnail: file.ThumbUrl,
			},
		},
		FID: file.Fid,
	}, nil
}
