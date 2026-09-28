// 收款码图片解析（docs/MIANQIAN.md §7.6 阶段一）。
//
// 决策 #8/#9：收款码获取 = 双保险（手动上传 + 监听端自动采集）
// 阶段一（最快跑通）：管理后台上传收款码图片 → 服务端解析二维码内容 → 绑定账户
// 用户从支付宝/微信 App「保存收款码」→ 上传 → 自动解析出 qr.alipay.com/xxx 或 weixin://wxpay/... 内容
package admin

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"paygateway/internal/admin/capture"
)

// maxQRUploadSize 收款码图片上传大小上限（2MB，对齐 capture.MaxQRUploadSize）
const maxQRUploadSize = capture.MaxQRUploadSize

// handleDecodeQRCode 上传收款码图片 → 解析二维码内容
// POST /api/admin/accounts/decode-qrcode  (multipart/form-data: file)
func (h *AccountHandler) handleDecodeQRCode(c *gin.Context) {
	// 限制上传大小（Go 1.19+ 自动处理超限）
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxQRUploadSize+1<<10)

	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少收款码图片（字段名 file）"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxQRUploadSize))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取图片失败: " + err.Error()})
		return
	}
	content, err := capture.DecodeQRContent(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"content": content}})
}
