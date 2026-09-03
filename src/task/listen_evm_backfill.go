package task

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/GMWalletApp/epusdt/model/service"
	"github.com/GMWalletApp/epusdt/util/log"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	evmBackfillPollInterval = 10 * time.Second
	evmBackfillJobTimeout   = 50 * time.Second
	evmBackfillRPCTimeout   = 12 * time.Second
	evmBackfillLookback     = uint64(1200)
	evmBackfillOverlap      = uint64(6)
	evmBackfillChunkSize    = uint64(200)
)

var evmBackfillNetworks = []string{
	mdb.NetworkBsc,
	mdb.NetworkEthereum,
	mdb.NetworkPolygon,
	mdb.NetworkPlasma,
}

type evmBackfillRPC interface {
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
	FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error)
}

type evmBackfillScanner struct {
	lastScanned map[string]uint64
}

var (
	gEvmBackfillJobLock sync.Mutex
	gEvmBackfillScanner = evmBackfillScanner{lastScanned: make(map[string]uint64)}
)

type EvmRpcBackfillJob struct{}

func (EvmRpcBackfillJob) Run() {
	if !gEvmBackfillJobLock.TryLock() {
		log.Sugar.Debug("[EVM-RPC] previous backfill is still running, skipping tick")
		return
	}
	defer gEvmBackfillJobLock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), evmBackfillJobTimeout)
	defer cancel()
	if err := gEvmBackfillScanner.pollOnce(ctx); err != nil {
		log.Sugar.Warnf("[EVM-RPC] backfill failed: %v", err)
	}
}

func (s *evmBackfillScanner) pollOnce(ctx context.Context) error {
	locks, err := data.ListActiveTransactionLocks(evmBackfillNetworks...)
	if err != nil {
		return err
	}
	recipients := groupEvmBackfillRecipients(locks)
	for _, network := range evmBackfillNetworks {
		if len(recipients[network]) == 0 || !data.IsChainEnabled(network) {
			continue
		}
		contracts := loadChainTokenContracts(network, "[EVM-RPC]")
		if len(contracts) == 0 {
			continue
		}
		if err := s.scanNetwork(ctx, network, contracts, recipients[network]); err != nil {
			log.Sugar.Warnf("[EVM-RPC-%s] scan failed: %v", network, err)
		}
	}
	return nil
}

func groupEvmBackfillRecipients(locks []data.ActiveTransactionLock) map[string][]common.Address {
	sets := make(map[string]map[common.Address]struct{})
	for _, lock := range locks {
		network := strings.ToLower(strings.TrimSpace(lock.Network))
		if !common.IsHexAddress(lock.Address) {
			continue
		}
		if sets[network] == nil {
			sets[network] = make(map[common.Address]struct{})
		}
		sets[network][common.HexToAddress(lock.Address)] = struct{}{}
	}
	out := make(map[string][]common.Address, len(sets))
	for network, set := range sets {
		for address := range set {
			out[network] = append(out[network], address)
		}
		sort.Slice(out[network], func(i, j int) bool {
			return strings.ToLower(out[network][i].Hex()) < strings.ToLower(out[network][j].Hex())
		})
	}
	return out
}

func (s *evmBackfillScanner) scanNetwork(ctx context.Context, network string, contracts, recipients []common.Address) error {
	nodes, err := listEvmBackfillNodes(network)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no enabled HTTP or WS RPC node configured")
	}

	var failures []string
	for _, node := range nodes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dialCtx, cancel := context.WithTimeout(ctx, evmBackfillRPCTimeout)
		client, dialErr := ethclient.DialContext(dialCtx, strings.TrimSpace(node.Url))
		cancel()
		if dialErr != nil {
			data.RecordRpcNodeFailure(node.ID)
			failures = append(failures, fmt.Sprintf("%s: dial: %v", data.RpcNodeLogLabel(node), dialErr))
			continue
		}

		scanErr := s.scanNetworkWithClient(ctx, network, client, contracts, recipients)
		client.Close()
		if scanErr != nil {
			data.RecordRpcNodeFailure(node.ID)
			failures = append(failures, fmt.Sprintf("%s: %v", data.RpcNodeLogLabel(node), scanErr))
			continue
		}
		data.RecordRpcSuccess(network)
		data.RecordRpcNodeSuccess(node.ID)
		return nil
	}
	return fmt.Errorf("all RPC nodes failed: %s", strings.Join(failures, "; "))
}

func listEvmBackfillNodes(network string) ([]mdb.RpcNode, error) {
	httpNodes, err := data.ListManualPaymentRpcCandidates(network, mdb.RpcNodeTypeHttp)
	if err != nil {
		return nil, err
	}
	wsNodes, err := data.ListGeneralRpcCandidates(network, mdb.RpcNodeTypeWs)
	if err != nil {
		return nil, err
	}
	out := make([]mdb.RpcNode, 0, len(httpNodes)+len(wsNodes))
	seen := make(map[uint64]struct{})
	for _, node := range append(httpNodes, wsNodes...) {
		if node.ID == 0 || strings.TrimSpace(node.Url) == "" || data.IsRpcNodeCoolingDown(node.ID) {
			continue
		}
		if _, ok := seen[node.ID]; ok {
			continue
		}
		seen[node.ID] = struct{}{}
		out = append(out, node)
	}
	return out, nil
}

func (s *evmBackfillScanner) scanNetworkWithClient(ctx context.Context, network string, client evmBackfillRPC, contracts, recipients []common.Address) error {
	rpcCtx, cancel := context.WithTimeout(ctx, evmBackfillRPCTimeout)
	latest, err := client.HeaderByNumber(rpcCtx, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("latest block: %w", err)
	}
	if latest == nil || latest.Number == nil || !latest.Number.IsUint64() {
		return fmt.Errorf("latest block number missing or invalid")
	}

	confirmations := uint64(1)
	chain, err := data.GetChainByNetwork(network)
	if err != nil {
		return fmt.Errorf("load chain settings: %w", err)
	}
	if chain != nil && chain.MinConfirmations > 1 {
		confirmations = uint64(chain.MinConfirmations)
	}
	latestNumber := latest.Number.Uint64()
	if latestNumber+1 < confirmations {
		return nil
	}
	scanHead := latestNumber - (confirmations - 1)
	fromBlock := evmBackfillStartBlock(s.lastScanned[network], scanHead)
	if fromBlock > scanHead {
		return nil
	}

	for chunkStart := fromBlock; chunkStart <= scanHead; {
		chunkEnd := chunkStart + evmBackfillChunkSize - 1
		if chunkEnd < chunkStart || chunkEnd > scanHead {
			chunkEnd = scanHead
		}
		query := buildEvmBackfillQuery(contracts, recipients, chunkStart, chunkEnd)
		rpcCtx, cancel = context.WithTimeout(ctx, evmBackfillRPCTimeout)
		logs, filterErr := client.FilterLogs(rpcCtx, query)
		cancel()
		if filterErr != nil {
			return fmt.Errorf("filter logs blocks %d-%d: %w", chunkStart, chunkEnd, filterErr)
		}
		if err := processEvmBackfillLogs(ctx, network, client, logs); err != nil {
			return err
		}
		if chunkEnd == scanHead {
			break
		}
		chunkStart = chunkEnd + 1
	}

	s.lastScanned[network] = scanHead
	data.RecordRpcBlockHeight(network, int64(scanHead))
	log.Sugar.Debugf("[EVM-RPC-%s] scanned confirmed blocks %d-%d for %d recipient(s)", network, fromBlock, scanHead, len(recipients))
	return nil
}

func evmBackfillStartBlock(lastScanned, scanHead uint64) uint64 {
	floor := uint64(0)
	if scanHead+1 > evmBackfillLookback {
		floor = scanHead - evmBackfillLookback + 1
	}
	if lastScanned == 0 {
		return floor
	}
	start := uint64(0)
	if lastScanned+1 > evmBackfillOverlap {
		start = lastScanned - evmBackfillOverlap + 1
	}
	if start < floor {
		return floor
	}
	return start
}

func buildEvmBackfillQuery(contracts, recipients []common.Address, fromBlock, toBlock uint64) ethereum.FilterQuery {
	recipientTopics := make([]common.Hash, 0, len(recipients))
	for _, recipient := range recipients {
		recipientTopics = append(recipientTopics, common.BytesToHash(recipient.Bytes()))
	}
	return ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Addresses: contracts,
		Topics: [][]common.Hash{
			{transferEventHash},
			nil,
			recipientTopics,
		},
	}
}

func processEvmBackfillLogs(ctx context.Context, network string, client evmBackfillRPC, logs []types.Log) error {
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		return logs[i].Index < logs[j].Index
	})
	blockTimes := make(map[uint64]int64)
	for _, event := range logs {
		if event.Removed || len(event.Topics) < 3 || event.Topics[0] != transferEventHash || len(event.Data) == 0 {
			continue
		}
		blockTimeMs, ok := blockTimes[event.BlockNumber]
		if !ok {
			rpcCtx, cancel := context.WithTimeout(ctx, evmBackfillRPCTimeout)
			header, err := client.HeaderByNumber(rpcCtx, new(big.Int).SetUint64(event.BlockNumber))
			cancel()
			if err != nil {
				return fmt.Errorf("block %d timestamp: %w", event.BlockNumber, err)
			}
			if header == nil {
				return fmt.Errorf("block %d timestamp missing", event.BlockNumber)
			}
			blockTimeMs = int64(header.Time) * 1000
			blockTimes[event.BlockNumber] = blockTimeMs
		}
		to := common.BytesToAddress(event.Topics[2].Bytes())
		amount := new(big.Int).SetBytes(event.Data)
		service.TryProcessEvmERC20Transfer(network, event.Address, to, amount, event.TxHash.Hex(), blockTimeMs)
	}
	return nil
}
