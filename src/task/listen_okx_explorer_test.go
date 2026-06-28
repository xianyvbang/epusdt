package task

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GMWalletApp/epusdt/internal/testutil"
	"github.com/GMWalletApp/epusdt/model/dao"
	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
)

func TestOkxChainName(t *testing.T) {
	tests := []struct {
		network string
		want    string
		ok      bool
	}{
		{network: mdb.NetworkBsc, want: "bsc", ok: true},
		{network: mdb.NetworkEthereum, want: "eth", ok: true},
		{network: mdb.NetworkPolygon, want: "polygon", ok: true},
		{network: mdb.NetworkPlasma, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.network, func(t *testing.T) {
			got, ok := okxChainName(tt.network)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("okxChainName(%q) = %q,%v want %q,%v", tt.network, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestBuildOkxExplorerHTMLURL(t *testing.T) {
	got, err := buildOkxExplorerHTMLURL(
		"https://web3.okx.com/zh-hans/explorer/bsc/address/{0}/token-transfer",
		mdb.NetworkBsc,
		"0xabc",
	)
	if err != nil {
		t.Fatalf("build page template url: %v", err)
	}
	want := "https://web3.okx.com/zh-hans/explorer/bsc/address/0xabc/token-transfer"
	if got != want {
		t.Fatalf("html url = %q, want %q", got, want)
	}

	got, err = buildOkxExplorerHTMLURL(
		"https://web3.okx.com/zh-hans/explorer/{chain}/address/{address}/token-transfer",
		mdb.NetworkEthereum,
		"0xdef",
	)
	if err != nil {
		t.Fatalf("build html template url: %v", err)
	}
	want = "https://web3.okx.com/zh-hans/explorer/eth/address/0xdef/token-transfer"
	if got != want {
		t.Fatalf("html url = %q, want %q", got, want)
	}
}

func TestParseOkxExplorerTransfers(t *testing.T) {
	body := []byte(`{
		"code": 0,
		"data": {
			"hits": [
				{
					"txhash": "0xpaid",
					"receive": "0x1111111111111111111111111111111111111111",
					"tokenSymbol": "usdt",
					"tokenContractAddress": "0x55d398326f99059ff775485246999027b3197955",
					"amount": "12.340000",
					"blocktime": "1800000000",
					"status": "success"
				},
				{
					"txHash": "0xfailed",
					"to": "0x1111111111111111111111111111111111111111",
					"symbol": "USDT",
					"value": "12.34",
					"status": "failed"
				}
			]
		}
	}`)

	transfers, err := ParseOkxExplorerTransfers(body, mdb.NetworkBsc)
	if err != nil {
		t.Fatalf("ParseOkxExplorerTransfers(): %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("transfers len = %d, want 1", len(transfers))
	}
	got := transfers[0]
	if got.TxHash != "0xpaid" || got.ToAddress != "0x1111111111111111111111111111111111111111" || got.TokenSymbol != "USDT" {
		t.Fatalf("unexpected transfer: %#v", got)
	}
	if got.Amount != 12.34 {
		t.Fatalf("amount = %v, want 12.34", got.Amount)
	}
	if got.BlockTimeMs != 1_800_000_000_000 {
		t.Fatalf("block time ms = %d, want 1800000000000", got.BlockTimeMs)
	}
}

func TestParseOkxExplorerTransfersOKLinkV5(t *testing.T) {
	body := []byte(`{
		"code": "0",
		"msg": "",
		"data": [
			{
				"transactionLists": [
					{
						"txId": "0xv5paid",
						"to": "0x1111111111111111111111111111111111111111",
						"transactionSymbol": "USDT",
						"tokenContractAddress": "0x55d398326f99059ff775485246999027b3197955",
						"amount": "12.340000000000000000",
						"transactionTime": "1800000000000",
						"state": "success"
					}
				]
			}
		]
	}`)

	transfers, err := ParseOkxExplorerTransfers(body, mdb.NetworkBsc)
	if err != nil {
		t.Fatalf("ParseOkxExplorerTransfers(): %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("transfers len = %d, want 1", len(transfers))
	}
	got := transfers[0]
	if got.TxHash != "0xv5paid" || got.TokenSymbol != "USDT" || got.Amount != 12.34 {
		t.Fatalf("unexpected transfer: %#v", got)
	}
}

func TestParseOkxExplorerTransfersHTMLState(t *testing.T) {
	body := []byte(`<html><body>
		<script type="application/json" id="appState">{
			"props": {
				"tokenTransferStore": {
					"list": [{
						"txHash": "0xhtmlpaid",
						"toAddress": "0x1111111111111111111111111111111111111111",
						"symbol": "USDT",
						"contractAddress": "0x55d398326f99059ff775485246999027b3197955",
						"tokenAmount": "12.340000",
						"blockTime": 1800000000000,
						"status": "success"
					}]
				}
			}
		}</script>
	</body></html>`)

	transfers, err := ParseOkxExplorerTransfers(body, mdb.NetworkBsc)
	if err != nil {
		t.Fatalf("ParseOkxExplorerTransfers(): %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("transfers len = %d, want 1", len(transfers))
	}
	got := transfers[0]
	if got.TxHash != "0xhtmlpaid" || got.TokenSymbol != "USDT" || got.Amount != 12.34 {
		t.Fatalf("unexpected transfer: %#v", got)
	}
}

func TestParseOkxExplorerTransfersAcceptsEmptyHTMLAndRejectsAPIError(t *testing.T) {
	transfers, err := ParseOkxExplorerTransfers([]byte("<html></html>"), mdb.NetworkBsc)
	if err != nil {
		t.Fatalf("empty html error = %v", err)
	}
	if len(transfers) != 0 {
		t.Fatalf("empty html transfers len = %d, want 0", len(transfers))
	}
	if _, err := ParseOkxExplorerTransfers([]byte(`{"code":5001,"msg":"API_KEY_NOT_FIND"}`), mdb.NetworkBsc); err == nil || !strings.Contains(err.Error(), "API_KEY_NOT_FIND") {
		t.Fatalf("api error = %v, want API_KEY_NOT_FIND", err)
	}
}

func TestParseOkxExplorerBrowserCapturePrefersXHRJSON(t *testing.T) {
	capture := okxBrowserCapture{
		ResponseBodies: [][]byte{[]byte(`{
			"code": "0",
			"data": [{
				"transactionLists": [{
					"txId": "0xxhrpaid",
					"to": "0x1111111111111111111111111111111111111111",
					"transactionSymbol": "USDT",
					"tokenContractAddress": "0x55d398326f99059ff775485246999027b3197955",
					"amount": "12.34",
					"transactionTime": 1800000000000,
					"state": "success"
				}]
			}]
		}`)},
		DOMRowsJSON: `[{"text":"0xdompaid 12.34 USDT 0x1111111111111111111111111111111111111111"}]`,
	}

	transfers := parseOkxExplorerBrowserCapture(capture, mdb.NetworkBsc, "0x1111111111111111111111111111111111111111")
	if len(transfers) != 1 {
		t.Fatalf("transfers len = %d, want 1", len(transfers))
	}
	if transfers[0].TxHash != "0xxhrpaid" {
		t.Fatalf("tx hash = %q, want xhr result", transfers[0].TxHash)
	}
}

func TestParseOkxExplorerBrowserCaptureFallsBackToDOMRows(t *testing.T) {
	tx := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	receive := "0x1111111111111111111111111111111111111111"
	capture := okxBrowserCapture{
		DOMRowsJSON: fmt.Sprintf(`[{
			"text": "2026-06-28 12:30:00 %s From 0x2222222222222222222222222222222222222222 To %s 12.34 USDT Success",
			"links": [{"href": "https://web3.okx.com/explorer/bsc/tx/%s", "text": "%s"}]
		}]`, tx, receive, tx, tx),
	}

	transfers := parseOkxExplorerBrowserCapture(capture, mdb.NetworkBsc, receive)
	if len(transfers) != 1 {
		t.Fatalf("transfers len = %d, want 1", len(transfers))
	}
	got := transfers[0]
	if got.TxHash != tx || got.ToAddress != receive || got.TokenSymbol != "USDT" || got.Amount != 12.34 || got.BlockTimeMs == 0 {
		t.Fatalf("unexpected transfer: %#v", got)
	}
}

func TestOkxExplorerScannerProcessesMatchingBscTransfer(t *testing.T) {
	cleanup := testutil.SetupTestDatabases(t)
	defer cleanup()

	const (
		tradeID = "okx_trade_paid"
		amount  = 12.34
	)
	receive := "0x1111111111111111111111111111111111111111"

	order := &mdb.Orders{
		TradeId:        tradeID,
		OrderId:        "okx_order_paid",
		Amount:         100,
		Currency:       "CNY",
		ActualAmount:   amount,
		Token:          "USDT",
		Network:        mdb.NetworkBsc,
		ReceiveAddress: receive,
		Status:         mdb.StatusWaitPay,
	}
	if err := dao.Mdb.Create(order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if err := data.LockTransaction(mdb.NetworkBsc, receive, "USDT", tradeID, amount, time.Hour); err != nil {
		t.Fatalf("lock transaction: %v", err)
	}

	node := mdb.RpcNode{
		Network: mdb.NetworkBsc,
		Url:     "https://web3.okx.com/zh-hans/explorer/bsc/address/{0}/token-transfer",
		Type:    mdb.RpcNodeTypeOkx,
		ApiKey:  "okx-test-key",
		Enabled: true,
		Purpose: mdb.RpcNodePurposeGeneral,
		Status:  mdb.RpcNodeStatusOk,
	}
	if err := dao.Mdb.Create(&node).Error; err != nil {
		t.Fatalf("seed okx rpc node: %v", err)
	}

	var gotURL, gotNetwork, gotAddress string
	scanner := okxExplorerScanner{
		browserFetcher: func(pageURL string, network string, address string) ([]OkxObservedTransfer, error) {
			gotURL = pageURL
			gotNetwork = network
			gotAddress = address
			return []OkxObservedTransfer{{
				Network:              network,
				TxHash:               "0xokxpaid",
				ToAddress:            address,
				TokenSymbol:          "USDT",
				TokenContractAddress: "0x55d398326f99059ff775485246999027b3197955",
				Amount:               amount,
				BlockTimeMs:          time.Now().UnixMilli(),
				Success:              true,
			}}, nil
		},
	}
	if err := scanner.pollOnce(); err != nil {
		t.Fatalf("pollOnce(): %v", err)
	}
	if gotURL != "https://web3.okx.com/zh-hans/explorer/bsc/address/"+receive+"/token-transfer" || gotNetwork != mdb.NetworkBsc || gotAddress != receive {
		t.Fatalf("browser fetch args url=%q network=%q address=%q", gotURL, gotNetwork, gotAddress)
	}

	paid, err := data.GetOrderInfoByTradeId(tradeID)
	if err != nil {
		t.Fatalf("load paid order: %v", err)
	}
	if paid.Status != mdb.StatusPaySuccess {
		t.Fatalf("order status = %d, want %d", paid.Status, mdb.StatusPaySuccess)
	}
	if paid.BlockTransactionId != "0xokxpaid" {
		t.Fatalf("block transaction id = %q, want 0xokxpaid", paid.BlockTransactionId)
	}
}
