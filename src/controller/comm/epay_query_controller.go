package comm

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/GMWalletApp/epusdt/middleware"
	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/labstack/echo/v4"
)

// epayOrderQueryResponse is the native response shape used by the EPay
// /api.php?act=order protocol. It deliberately does not use the project's
// standard status_code/data envelope.
type epayOrderQueryResponse struct {
	Code        int    `json:"code"`
	Msg         string `json:"msg"`
	TradeNo     string `json:"trade_no,omitempty"`
	OutTradeNo  string `json:"out_trade_no,omitempty"`
	Type        string `json:"type,omitempty"`
	Pid         string `json:"pid,omitempty"`
	AddTime     string `json:"addtime,omitempty"`
	EndTime     string `json:"endtime"`
	Name        string `json:"name,omitempty"`
	Money       string `json:"money,omitempty"`
	Status      int    `json:"status"`
	Param       string `json:"param"`
	Buyer       string `json:"buyer"`
}

// QueryEPayOrder implements the traditional EPay order query endpoint:
// /api.php?act=order&pid={pid}&key={secret}&trade_no=... or out_trade_no=...
//
// Authentication is intentionally based on the submitted merchant key, as
// required by the EPay protocol. The order must also belong to that merchant
// API key, so a valid key cannot query another merchant's order.
func (c *BaseCommController) QueryEPayOrder(ctx echo.Context) error {
	param := func(name string) string {
		value := ctx.QueryParam(name)
		if value == "" && ctx.Request().Method == http.MethodPost {
			value = ctx.FormValue(name)
		}
		return value
	}
	if !strings.EqualFold(strings.TrimSpace(param("act")), "order") {
		return ctx.JSON(http.StatusOK, epayOrderQueryResponse{Code: 0, Msg: "参数错误！"})
	}

	pid := strings.TrimSpace(param("pid"))
	key := param("key")
	tradeNo := strings.TrimSpace(param("trade_no"))
	outTradeNo := strings.TrimSpace(param("out_trade_no"))
	if pid == "" || key == "" || (tradeNo == "" && outTradeNo == "") {
		return ctx.JSON(http.StatusOK, epayOrderQueryResponse{Code: 0, Msg: "参数错误！"})
	}

	apiKey, err := data.GetEnabledApiKey(pid)
	if err != nil || apiKey == nil || apiKey.ID == 0 || subtle.ConstantTimeCompare([]byte(key), []byte(apiKey.SecretKey)) != 1 {
		return ctx.JSON(http.StatusOK, epayOrderQueryResponse{Code: 0, Msg: "签名错误！"})
	}
	if !middleware.IsIPWhitelisted(apiKey.IpWhitelist, ctx.RealIP()) {
		return ctx.JSON(http.StatusOK, epayOrderQueryResponse{Code: 0, Msg: "签名错误！"})
	}
	_ = data.TouchApiKeyUsage(apiKey.ID)

	var order *mdb.Orders
	if tradeNo != "" {
		order, err = data.GetOrderInfoByTradeId(tradeNo)
	} else {
		order, err = data.GetOrderInfoByOrderId(outTradeNo)
	}
	if err != nil || order == nil || order.ID == 0 || order.ApiKeyID != apiKey.ID {
		return ctx.JSON(http.StatusOK, epayOrderQueryResponse{Code: 0, Msg: "订单不存在！"})
	}

	orderType := strings.TrimSpace(order.EpayType)
	if orderType == "" {
		orderType = "alipay"
	}
	endTime := ""
	if order.Status == mdb.StatusPaySuccess {
		endTime = order.UpdatedAt.ToDateTimeString()
	}
	return ctx.JSON(http.StatusOK, epayOrderQueryResponse{
		Code:       1,
		Msg:        "查询订单号成功！",
		TradeNo:    order.TradeId,
		OutTradeNo: order.OrderId,
		Type:       orderType,
		Pid:        pid,
		AddTime:    order.CreatedAt.ToDateTimeString(),
		EndTime:    endTime,
		Name:       order.Name,
		Money:      fmt.Sprintf("%.2f", order.Amount),
		Status:     boolToEPayStatus(order.Status),
		Param:      "",
		Buyer:      "",
	})
}

func boolToEPayStatus(status int) int {
	if status == mdb.StatusPaySuccess {
		return 1
	}
	return 0
}
