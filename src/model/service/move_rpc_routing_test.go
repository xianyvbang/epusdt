package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GMWalletApp/epusdt/internal/testutil"
	"github.com/GMWalletApp/epusdt/model/dao"
	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	addressutil "github.com/GMWalletApp/epusdt/util/address"
	"github.com/dromara/carbon/v2"
)

func overrideAptosFullnodeURLForTest(t *testing.T, url string) {
	t.Helper()
	oldURL := aptosFullnodeURL
	aptosFullnodeURL = url
	aptosFixedNodeLogOnce = sync.Once{}
	t.Cleanup(func() {
		aptosFullnodeURL = oldURL
		aptosFixedNodeLogOnce = sync.Once{}
	})
}

func TestAptosGetLedgerVersionUsesFixedPublicNode(t *testing.T) {
	cleanup := testutil.SetupTestDatabases(t)
	defer cleanup()
	data.ResetRpcFailoverForTest()
	t.Cleanup(data.ResetRpcFailoverForTest)

	var fixedCalls int
	fixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixedCalls++
		if r.URL.Path != "/v1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ledger_version":"123"}`))
	}))
	defer fixed.Close()
	overrideAptosFullnodeURLForTest(t, fixed.URL)

	rows := []mdb.RpcNode{
		{Network: mdb.NetworkAptos, Url: "https://old-aptos-rpc.example.com", Type: mdb.RpcNodeTypeHttp, Weight: 100, Enabled: true, Purpose: mdb.RpcNodePurposeBoth, Status: mdb.RpcNodeStatusOk},
	}
	if err := dao.Mdb.Create(&rows).Error; err != nil {
		t.Fatalf("seed aptos rpc_nodes: %v", err)
	}

	got, err := AptosGetLedgerVersion()
	if err != nil {
		t.Fatalf("AptosGetLedgerVersion(): %v", err)
	}
	if got != 123 {
		t.Fatalf("ledger version = %d, want 123", got)
	}
	if fixedCalls != 1 {
		t.Fatalf("fixed node calls = %d, want 1", fixedCalls)
	}
}

func TestValidateManualAptosPaymentUsesFixedPublicNode(t *testing.T) {
	cleanup := testutil.SetupTestDatabases(t)
	defer cleanup()

	if err := dao.Mdb.Create(&mdb.Chain{Network: mdb.NetworkAptos, Enabled: true, MinConfirmations: 1}).Error; err != nil {
		t.Fatalf("create aptos chain: %v", err)
	}
	const usdc = "0xbae207659db88bea0cbead6da0ed00aac12edcdda169e591cd41c94180b46f3b"
	upsertTestChainToken(t, mdb.ChainToken{
		Network:         mdb.NetworkAptos,
		Symbol:          "USDC",
		ContractAddress: usdc,
		Decimals:        6,
		Enabled:         true,
	})

	receive, err := addressutil.NormalizeMoveAddress("0xa")
	if err != nil {
		t.Fatalf("normalize aptos receive: %v", err)
	}
	store, err := addressutil.NormalizeMoveAddress("0x31")
	if err != nil {
		t.Fatalf("normalize aptos store: %v", err)
	}
	senderStore, err := addressutil.NormalizeMoveAddress("0x32")
	if err != nil {
		t.Fatalf("normalize aptos sender store: %v", err)
	}

	var fixedCalls int
	fixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixedCalls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/transactions/by_hash/"):
			_, _ = w.Write([]byte(`{"type":"user_transaction","success":true,"hash":"0xabc","version":"101","timestamp":"1700000000123456","payload":{"function":"0x1::primary_fungible_store::transfer","arguments":[{"inner":"` + usdc + `"},"` + receive + `","4200000"],"type":"entry_function_payload"},"events":[{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"4200000","store":"` + senderStore + `"}},{"type":"0x1::fungible_asset::Deposit","data":{"amount":"4200000","store":"` + store + `"}}],"changes":[{"address":"` + senderStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdc + `"}}},"type":"write_resource"},{"address":"` + store + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdc + `"}}},"type":"write_resource"},{"address":"` + store + `","data":{"type":"0x1::object::ObjectCore","data":{"owner":"` + receive + `"}},"type":"write_resource"}]}`))
		case r.URL.Path == "/v1":
			_, _ = w.Write([]byte(`{"ledger_version":"101"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixed.Close()
	overrideAptosFullnodeURLForTest(t, fixed.URL)

	rows := []mdb.RpcNode{
		{Network: mdb.NetworkAptos, Url: "https://old-both.example.com", Type: mdb.RpcNodeTypeHttp, Weight: 100, Enabled: true, Purpose: mdb.RpcNodePurposeBoth, Status: mdb.RpcNodeStatusOk},
		{Network: mdb.NetworkAptos, Url: "https://old-manual.example.com", Type: mdb.RpcNodeTypeHttp, Weight: 1, Enabled: true, Purpose: mdb.RpcNodePurposeManualVerify, Status: mdb.RpcNodeStatusOk},
	}
	if err := dao.Mdb.Create(&rows).Error; err != nil {
		t.Fatalf("seed aptos rpc_nodes: %v", err)
	}

	order := &mdb.Orders{
		BaseModel:      mdb.BaseModel{ID: 1, CreatedAt: *carbon.NewTime(carbon.CreateFromTimestampMilli(1699999999000))},
		Network:        mdb.NetworkAptos,
		Token:          "USDC",
		ActualAmount:   4.2,
		ReceiveAddress: receive,
	}
	got, err := ValidateManualAptosPayment(order, "ABC")
	if err != nil {
		t.Fatalf("ValidateManualAptosPayment(): %v", err)
	}
	if got != "0xabc" {
		t.Fatalf("canonical tx id = %q, want %q", got, "0xabc")
	}
	if fixedCalls != 2 {
		t.Fatalf("fixed node calls = %d, want 2", fixedCalls)
	}
}
