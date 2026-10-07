package task

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/GMWalletApp/epusdt/internal/testutil"
	"github.com/GMWalletApp/epusdt/model/dao"
	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Exercise the real ethclient and scanner through an in-process JSON-RPC server.
type fakeEvmBackfillRPC struct {
	latest    int64
	filterErr error
	stopAfter int
	queries   []map[string]interface{}
}

func (f *fakeEvmBackfillRPC) GetBlockByNumber(context.Context, string, bool) (*types.Header, error) {
	if len(f.queries) >= f.stopAfter {
		return nil, errors.New("test scan complete")
	}
	return &types.Header{Number: big.NewInt(f.latest), Difficulty: big.NewInt(0), Time: 1}, nil
}

func (f *fakeEvmBackfillRPC) GetLogs(_ context.Context, query map[string]interface{}) ([]types.Log, error) {
	f.queries = append(f.queries, query)
	return []types.Log{}, f.filterErr
}

func TestConfirmedEvmHead(t *testing.T) {
	for _, tt := range []struct {
		name          string
		head          int64
		confirmations int
		want          int64
	}{
		{"no confirmations", 5002, 0, 5002},
		{"one confirmation", 5002, 1, 5002},
		{"three confirmations", 5002, 3, 5000},
		{"before confirmed head", 1, 3, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := confirmedEvmHead(tt.head, tt.confirmations); got != tt.want {
				t.Fatalf("confirmed head = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEvmRecipientTopicsNormalizesAndDeduplicates(t *testing.T) {
	address := "0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a"
	got := evmRecipientTopicsFromWallets([]mdb.WalletAddress{
		{Address: " " + address + " "},
		{Address: strings.ToUpper(address)},
		{Address: "invalid"},
	})
	if len(got) != 1 || got[0] != common.BytesToHash(common.HexToAddress(address).Bytes()) {
		t.Fatalf("unexpected recipient topics: %v", got)
	}
}

func TestEvmBackfillScanRangesAndPersistedCursor(t *testing.T) {
	for _, tt := range []struct {
		name    string
		network string
		cursor  int64
		head    int64
		ranges  [][2]int64
	}{
		{"initial lookback", mdb.NetworkEthereum, 0, 5002, [][2]int64{{2953, 3952}, {3953, 4952}, {4953, 5000}}},
		{"initial short chain", mdb.NetworkEthereum, 0, 102, [][2]int64{{1, 100}}},
		{"bsc batches", mdb.NetworkBsc, 4500, 5002, [][2]int64{{4501, 4700}, {4701, 4900}, {4901, 5000}}},
		{"stale cursor retained", mdb.NetworkEthereum, 1000, 5002, [][2]int64{{1001, 2000}, {2001, 3000}, {3001, 4000}, {4001, 5000}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := testutil.SetupTestDatabases(t)
			defer cleanup()
			if tt.cursor != 0 {
				if err := data.UpsertEvmScanCursor(tt.network, tt.cursor); err != nil {
					t.Fatal(err)
				}
			}
			fake := &fakeEvmBackfillRPC{latest: tt.head, stopAfter: len(tt.ranges)}
			err := runTestEvmBackfill(t, tt.network, fake)
			if err == nil || !strings.Contains(err.Error(), "test scan complete") {
				t.Fatalf("unexpected scan result: %v", err)
			}
			assertEvmBackfillQueries(t, fake.queries, tt.ranges)
			assertEvmBackfillCursor(t, tt.network, tt.head-2)

			// A new scanner must resume from the persisted confirmed head.
			next := &fakeEvmBackfillRPC{latest: tt.head + 3, stopAfter: 1}
			if err := runTestEvmBackfill(t, tt.network, next); err == nil || !strings.Contains(err.Error(), "test scan complete") {
				t.Fatalf("resume scan: %v", err)
			}
			assertEvmBackfillQueries(t, next.queries, [][2]int64{{tt.head - 1, tt.head + 1}})
			assertEvmBackfillCursor(t, tt.network, tt.head+1)
		})
	}
}

func TestEvmBackfillRPCErrorDoesNotAdvanceCursor(t *testing.T) {
	cleanup := testutil.SetupTestDatabases(t)
	defer cleanup()
	if err := data.UpsertEvmScanCursor(mdb.NetworkBsc, 4000); err != nil {
		t.Fatal(err)
	}
	fake := &fakeEvmBackfillRPC{latest: 5002, stopAfter: 1, filterErr: errors.New("temporary RPC failure")}
	if err := runTestEvmBackfill(t, mdb.NetworkBsc, fake); err == nil || !strings.Contains(err.Error(), "temporary RPC failure") {
		t.Fatalf("expected filter error, got %v", err)
	}
	assertEvmBackfillQueries(t, fake.queries, [][2]int64{{4001, 4200}})
	assertEvmBackfillCursor(t, mdb.NetworkBsc, 4000)
}

func runTestEvmBackfill(t *testing.T, network string, fake *fakeEvmBackfillRPC) error {
	t.Helper()
	if err := data.UpdateChainFields(network, map[string]interface{}{"min_confirmations": 3, "scan_interval_sec": 1}); err != nil {
		t.Fatal(err)
	}
	if err := dao.Mdb.Where("network = ?", network).FirstOrCreate(&mdb.WalletAddress{
		Network: network,
		Address: "0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a",
		Status:  mdb.TokenStatusEnable,
	}).Error; err != nil {
		t.Fatal(err)
	}
	server := rpc.NewServer()
	defer server.Stop()
	if err := server.RegisterName("eth", fake); err != nil {
		t.Fatal(err)
	}
	client := ethclient.NewClient(rpc.DialInProc(server))
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return runEvmBackfillLoop(ctx, client, network, "[TEST]", mdb.RpcNode{}, ethereum.FilterQuery{
		Addresses: []common.Address{common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")},
	}, func(common.Address) bool { return true })
}

func assertEvmBackfillCursor(t *testing.T, network string, want int64) {
	t.Helper()
	got, initialized, err := loadEvmScanCursor(network)
	if err != nil || !initialized || got != want {
		t.Fatalf("cursor = %d, initialized = %t, error = %v; want %d", got, initialized, err, want)
	}
}

func assertEvmBackfillQueries(t *testing.T, queries []map[string]interface{}, ranges [][2]int64) {
	t.Helper()
	if len(queries) != len(ranges) {
		t.Fatalf("query count = %d, want %d", len(queries), len(ranges))
	}
	for i, query := range queries {
		if query["fromBlock"] != hexutil.EncodeUint64(uint64(ranges[i][0])) || query["toBlock"] != hexutil.EncodeUint64(uint64(ranges[i][1])) {
			t.Fatalf("query %d range = %v-%v, want %v", i, query["fromBlock"], query["toBlock"], ranges[i])
		}
		addresses, ok := query["address"].([]interface{})
		if !ok || len(addresses) != 1 || addresses[0] != "0x55d398326f99059ff775485246999027b3197955" {
			t.Fatalf("unexpected contracts: %v", query["address"])
		}
		topics, ok := query["topics"].([]interface{})
		if !ok || len(topics) != 3 || topics[1] != nil {
			t.Fatalf("unexpected topics: %v", query["topics"])
		}
		events, ok := topics[0].([]interface{})
		if !ok || len(events) != 1 || events[0] != transferEventHash.Hex() {
			t.Fatalf("unexpected event topic: %v", topics[0])
		}
		recipients, ok := topics[2].([]interface{})
		wantRecipient := common.BytesToHash(common.HexToAddress("0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a").Bytes()).Hex()
		if !ok || len(recipients) != 1 || recipients[0] != wantRecipient {
			t.Fatalf("unexpected recipient topic: %v", topics[2])
		}
	}
}
