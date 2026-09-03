package task

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/GMWalletApp/epusdt/internal/testutil"
	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type fakeEvmBackfillRPC struct {
	latest    uint64
	filterErr error
	queries   []ethereum.FilterQuery
}

func (f *fakeEvmBackfillRPC) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	if number == nil {
		return &types.Header{Number: new(big.Int).SetUint64(f.latest)}, nil
	}
	return &types.Header{Number: new(big.Int).Set(number), Time: uint64(time.Now().Unix())}, nil
}

func (f *fakeEvmBackfillRPC) FilterLogs(_ context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	f.queries = append(f.queries, query)
	if f.filterErr != nil {
		return nil, f.filterErr
	}
	return nil, nil
}

func TestEvmBackfillStartBlock(t *testing.T) {
	tests := []struct {
		name        string
		lastScanned uint64
		scanHead    uint64
		want        uint64
	}{
		{name: "initial lookback", scanHead: 5000, want: 3801},
		{name: "initial before lookback", scanHead: 500, want: 0},
		{name: "overlap", lastScanned: 4990, scanHead: 5000, want: 4985},
		{name: "stale cursor capped", lastScanned: 1000, scanHead: 5000, want: 3801},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evmBackfillStartBlock(tt.lastScanned, tt.scanHead); got != tt.want {
				t.Fatalf("start block = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildEvmBackfillQueryFiltersTransferRecipient(t *testing.T) {
	contract := common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	recipient := common.HexToAddress("0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a")
	query := buildEvmBackfillQuery([]common.Address{contract}, []common.Address{recipient}, 100, 200)
	if query.FromBlock.Cmp(big.NewInt(100)) != 0 || query.ToBlock.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("unexpected block range %s-%s", query.FromBlock, query.ToBlock)
	}
	if len(query.Addresses) != 1 || query.Addresses[0] != contract {
		t.Fatalf("unexpected contracts: %v", query.Addresses)
	}
	if len(query.Topics) != 3 || len(query.Topics[0]) != 1 || query.Topics[0][0] != transferEventHash {
		t.Fatalf("unexpected event topics: %v", query.Topics)
	}
	if len(query.Topics[2]) != 1 || common.BytesToAddress(query.Topics[2][0].Bytes()) != recipient {
		t.Fatalf("unexpected recipient topic: %v", query.Topics[2])
	}
}

func TestGroupEvmBackfillRecipientsNormalizesAndDeduplicates(t *testing.T) {
	address := "0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a"
	locks := []data.ActiveTransactionLock{
		{Network: mdb.NetworkBsc, Address: address, ExpiresAt: time.Now().Add(time.Minute)},
		{Network: mdb.NetworkBsc, Address: strings.ToUpper(address), ExpiresAt: time.Now().Add(time.Minute)},
		{Network: mdb.NetworkBsc, Address: "invalid", ExpiresAt: time.Now().Add(time.Minute)},
	}
	got := groupEvmBackfillRecipients(locks)
	if len(got[mdb.NetworkBsc]) != 1 || got[mdb.NetworkBsc][0] != common.HexToAddress(address) {
		t.Fatalf("unexpected recipients: %v", got)
	}
}

func TestEvmBackfillRPCErrorDoesNotAdvanceCursor(t *testing.T) {
	cleanup := testutil.SetupTestDatabases(t)
	defer cleanup()

	scanner := evmBackfillScanner{lastScanned: map[string]uint64{mdb.NetworkBsc: 4000}}
	client := &fakeEvmBackfillRPC{latest: 5002, filterErr: errors.New("temporary RPC failure")}
	err := scanner.scanNetworkWithClient(
		context.Background(),
		mdb.NetworkBsc,
		client,
		[]common.Address{common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")},
		[]common.Address{common.HexToAddress("0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a")},
	)
	if err == nil {
		t.Fatal("expected filter error")
	}
	if got := scanner.lastScanned[mdb.NetworkBsc]; got != 4000 {
		t.Fatalf("cursor advanced to %d after RPC error, want 4000", got)
	}
}

func TestEvmBackfillSuccessAdvancesCursorWithNextScanOverlap(t *testing.T) {
	cleanup := testutil.SetupTestDatabases(t)
	defer cleanup()
	if err := data.UpdateChainFields(mdb.NetworkBsc, map[string]interface{}{"min_confirmations": 3}); err != nil {
		t.Fatalf("set confirmations: %v", err)
	}

	scanner := evmBackfillScanner{lastScanned: make(map[string]uint64)}
	contracts := []common.Address{common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")}
	recipients := []common.Address{common.HexToAddress("0x1a1f3c8e3a0cf34c66c752d2149436aa1dc09a3a")}
	first := &fakeEvmBackfillRPC{latest: 5002}
	if err := scanner.scanNetworkWithClient(context.Background(), mdb.NetworkBsc, first, contracts, recipients); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if got := scanner.lastScanned[mdb.NetworkBsc]; got != 5000 {
		t.Fatalf("cursor = %d, want confirmed head 5000", got)
	}

	second := &fakeEvmBackfillRPC{latest: 5005}
	if err := scanner.scanNetworkWithClient(context.Background(), mdb.NetworkBsc, second, contracts, recipients); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(second.queries) != 1 {
		t.Fatalf("second scan query count = %d, want 1", len(second.queries))
	}
	if got := second.queries[0].FromBlock.Uint64(); got != 4995 {
		t.Fatalf("second scan starts at %d, want overlap start 4995", got)
	}
}
