package service

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/GMWalletApp/epusdt/model/mdb"
	addressutil "github.com/GMWalletApp/epusdt/util/address"
	"github.com/dromara/carbon/v2"
)

func TestParseAptosTransfersUSDTAndUSDCFungibleEvents(t *testing.T) {
	receive, _ := addressutil.NormalizeMoveAddress("0xa")
	receive2, _ := addressutil.NormalizeMoveAddress("0xb")
	usdc := "0xbae207659db88bea0cbead6da0ed00aac12edcdda169e591cd41c94180b46f3b"
	usdt := "0x357b0b74bc833e95a115ad22604854d6b0fca151cecd94111770e5d6ffc9dc2b"
	usdcStore, _ := addressutil.NormalizeMoveAddress("0x12")
	usdcSenderStore, _ := addressutil.NormalizeMoveAddress("0x13")
	usdtStore, _ := addressutil.NormalizeMoveAddress("0x14")
	usdtSenderStore, _ := addressutil.NormalizeMoveAddress("0x15")
	noOwnerStore, _ := addressutil.NormalizeMoveAddress("0x16")
	body := []byte(`[
		{
			"type":"user_transaction",
			"success":true,
			"hash":"abc",
			"version":"101",
			"timestamp":"1700000000123456",
			"events":[
				{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"2500000","store":"` + usdcSenderStore + `"}},
				{"type":"0x1::fungible_asset::Deposit","data":{"amount":"2500000","store":"` + usdcStore + `"}}
			],
			"changes":[
				{"address":"` + usdcSenderStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdc + `"}}},"type":"write_resource"},
				{"address":"` + usdcStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdc + `"}}},"type":"write_resource"},
				{"address":"` + usdcStore + `","data":{"type":"0x1::object::ObjectCore","data":{"owner":"0xa"}},"type":"write_resource"}
			]
		},
		{
			"type":"user_transaction",
			"success":true,
			"hash":"def",
			"version":"102",
			"timestamp":"1700000001123456",
			"payload":{"function":"0x1::primary_fungible_store::transfer","arguments":[{"inner":"` + usdt + `"},"` + receive2 + `","3100000"],"type":"entry_function_payload"},
			"events":[
				{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"3100000","store":"` + usdtSenderStore + `"}},
				{"type":"0x1::fungible_asset::Deposit","data":{"amount":"3100000","store":"` + usdtStore + `"}}
			],
			"changes":[
				{"address":"` + usdtSenderStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + usdtStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + usdtStore + `","data":{"type":"0x1::object::ObjectCore","data":{"owner":"` + receive2 + `"}},"type":"write_resource"}
			]
		},
		{
			"type":"user_transaction",
			"success":true,
			"hash":"skip-withdraw-only",
			"version":"103",
			"timestamp":"1700000002123456",
			"events":[
				{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"4100000","metadata_address":"` + usdc + `"}}
			]
		},
		{
			"type":"user_transaction",
			"success":true,
			"hash":"skip-no-owner",
			"version":"104",
			"timestamp":"1700000003123456",
			"events":[
				{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"5100000","store":"` + usdcSenderStore + `"}},
				{"type":"0x1::fungible_asset::Deposit","data":{"amount":"5100000","store":"` + noOwnerStore + `"}}
			],
			"changes":[
				{"address":"` + noOwnerStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdc + `"}}},"type":"write_resource"}
			]
		},
		{
			"type":"user_transaction",
			"success":false,
			"hash":"skip-failed",
			"events":[
				{"type":"0x1::fungible_asset::Deposit","data":{"to":"` + receive + `","amount":"1","metadata_address":"` + usdc + `"}}
			]
		},
		{
			"type":"user_transaction",
			"success":true,
			"hash":"skip-non-store-owner-fallback",
			"version":"105",
			"timestamp":"1700000004123456",
			"events":[
				{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"6100000","metadata_address":"` + usdc + `"}},
				{"type":"0x1::fungible_asset::Deposit","data":{"to":"` + receive + `","amount":"6100000","metadata_address":"` + usdc + `"}}
			]
		}
	]`)
	tokens := []mdb.ChainToken{
		{Network: mdb.NetworkAptos, Symbol: "USDC", ContractAddress: usdc, Decimals: 6, Enabled: true, MinAmount: 1},
		{Network: mdb.NetworkAptos, Symbol: "USDT", ContractAddress: usdt, Decimals: 6, Enabled: true, MinAmount: 3},
	}

	got, err := ParseAptosTransfers(body, receive, tokens)
	if err != nil {
		t.Fatalf("ParseAptosTransfers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("transfers len = %d, want 1: %#v", len(got), got)
	}
	if got[0].Token != "USDC" || got[0].Amount != 2.5 || got[0].TxID != "0xabc" || got[0].ReceiveAddress != receive {
		t.Fatalf("usdc transfer = %#v", got[0])
	}
	if got[0].TransferKey == "" {
		t.Fatalf("transfer key should be non-empty: %#v", got[0])
	}
	if got[0].MinAmount != 1 {
		t.Fatalf("usdc min amount = %.2f, want 1", got[0].MinAmount)
	}

	gotSecondWallet, err := ParseAptosTransfers(body, receive2, tokens)
	if err != nil {
		t.Fatalf("ParseAptosTransfers second wallet: %v", err)
	}
	if len(gotSecondWallet) != 1 || gotSecondWallet[0].Token != "USDT" || gotSecondWallet[0].Amount != 3.1 || gotSecondWallet[0].TxID != "0xdef" {
		t.Fatalf("usdt transfer = %#v", gotSecondWallet)
	}
}

func TestParseAptosTransfersSplitDepositsMatchSingleWithdraw(t *testing.T) {
	receive, _ := addressutil.NormalizeMoveAddress("0xa")
	receive2, _ := addressutil.NormalizeMoveAddress("0xb")
	usdt := "0x357b0b74bc833e95a115ad22604854d6b0fca151cecd94111770e5d6ffc9dc2b"
	withdrawStore, _ := addressutil.NormalizeMoveAddress("0x21")
	depositStore, _ := addressutil.NormalizeMoveAddress("0x22")
	depositStore2, _ := addressutil.NormalizeMoveAddress("0x23")
	body := []byte(`[
		{
			"type":"user_transaction",
			"success":true,
			"hash":"split",
			"version":"201",
			"timestamp":"1700000000123456",
			"events":[
				{"type":"0x1::fungible_asset::Withdraw","data":{"amount":"3000000","store":"` + withdrawStore + `"}},
				{"type":"0x1::fungible_asset::Deposit","data":{"amount":"1000000","store":"` + depositStore + `"}},
				{"type":"0x1::fungible_asset::Deposit","data":{"amount":"2000000","store":"` + depositStore2 + `"}}
			],
			"changes":[
				{"address":"` + withdrawStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + depositStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + depositStore + `","data":{"type":"0x1::object::ObjectCore","data":{"owner":"` + receive + `"}},"type":"write_resource"},
				{"address":"` + depositStore2 + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + depositStore2 + `","data":{"type":"0x1::object::ObjectCore","data":{"owner":"` + receive2 + `"}},"type":"write_resource"}
			]
		}
	]`)
	tokens := []mdb.ChainToken{
		{Network: mdb.NetworkAptos, Symbol: "USDT", ContractAddress: usdt, Decimals: 6, Enabled: true},
	}
	wallets := map[string]struct{}{receive: {}, receive2: {}}

	got, err := ParseAptosTransfersForWallets(body, wallets, tokens)
	if err != nil {
		t.Fatalf("ParseAptosTransfersForWallets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("transfers len = %d, want 2: %#v", len(got), got)
	}
	if got[0].Token != "USDT" || got[0].Amount != 1 || got[0].ReceiveAddress != receive || got[0].TxID != "0xsplit" {
		t.Fatalf("first split transfer = %#v", got[0])
	}
	if got[1].Token != "USDT" || got[1].Amount != 2 || got[1].ReceiveAddress != receive2 || got[1].TxID != "0xsplit" {
		t.Fatalf("second split transfer = %#v", got[1])
	}
}

func TestParseAptosTransfersUsesStoreOwnerWhenGuidAccountIsZero(t *testing.T) {
	receive, _ := addressutil.NormalizeMoveAddress("0xa")
	usdt := "0x357b0b74bc833e95a115ad22604854d6b0fca151cecd94111770e5d6ffc9dc2b"
	store, _ := addressutil.NormalizeMoveAddress("0x31")
	senderStore, _ := addressutil.NormalizeMoveAddress("0x32")
	body := []byte(`[
		{
			"type":"user_transaction",
			"success":true,
			"hash":"decimal",
			"version":"301",
			"timestamp":"1700000000123456",
			"events":[
				{"guid":{"creation_number":"0","account_address":"0x0"},"sequence_number":"0","type":"0x1::fungible_asset::Withdraw","data":{"amount":"10000","store":"` + senderStore + `"}},
				{"guid":{"creation_number":"0","account_address":"0x0"},"sequence_number":"0","type":"0x1::fungible_asset::Deposit","data":{"amount":"10000","store":"` + store + `"}}
			],
			"changes":[
				{"address":"` + senderStore + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + store + `","data":{"type":"0x1::fungible_asset::FungibleStore","data":{"metadata":{"inner":"` + usdt + `"}}},"type":"write_resource"},
				{"address":"` + store + `","data":{"type":"0x1::object::ObjectCore","data":{"owner":"` + receive + `"}},"type":"write_resource"}
			]
		}
	]`)
	tokens := []mdb.ChainToken{
		{Network: mdb.NetworkAptos, Symbol: "USDT", ContractAddress: usdt, Decimals: 6, Enabled: true},
	}

	got, err := ParseAptosTransfers(body, receive, tokens)
	if err != nil {
		t.Fatalf("ParseAptosTransfers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("transfers len = %d, want 1: %#v", len(got), got)
	}
	if got[0].ReceiveAddress != receive || got[0].Decimals != 6 || got[0].Amount != 0.01 || got[0].RawAmount.String() != "10000" {
		t.Fatalf("transfer amount = %#v", got[0])
	}
}

func TestEnsureMoveTransferMatchesOrder(t *testing.T) {
	receive, _ := addressutil.NormalizeMoveAddress("0xc")
	order := &mdb.Orders{
		BaseModel:      mdb.BaseModel{ID: 1, CreatedAt: *carbon.NewTime(carbon.CreateFromTimestampMilli(time.Now().Add(-time.Minute).UnixMilli()))},
		Network:        mdb.NetworkAptos,
		Token:          "USDC",
		ActualAmount:   3.5,
		ReceiveAddress: strings.ToUpper(receive),
	}
	transfer := MoveObservedTransfer{
		Network:        mdb.NetworkAptos,
		ReceiveAddress: receive,
		Token:          "USDC",
		RawAmount:      big.NewInt(3_500_000),
		Decimals:       6,
		BlockTimeMs:    time.Now().UnixMilli(),
	}
	if err := EnsureMoveTransferMatchesOrder(order, transfer); err != nil {
		t.Fatalf("EnsureMoveTransferMatchesOrder: %v", err)
	}

	transfer.RawAmount = big.NewInt(3_400_000)
	if err := EnsureMoveTransferMatchesOrder(order, transfer); err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("amount mismatch err = %v, want amount error", err)
	}
}
