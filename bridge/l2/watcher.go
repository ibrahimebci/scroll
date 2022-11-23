package l2

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/scroll-tech/go-ethereum"
	"github.com/scroll-tech/go-ethereum/accounts/abi"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/ethclient"
	"github.com/scroll-tech/go-ethereum/event"
	"github.com/scroll-tech/go-ethereum/log"

	bridge_abi "scroll-tech/bridge/abi"

	"scroll-tech/database"
	"scroll-tech/database/orm"
)

const (
	// keccak256("SentMessage(address,address,uint256,uint256,uint256,bytes,uint256,uint256)")
	sentMessageEventSignature = "806b28931bc6fbe6c146babfb83d5c2b47e971edb43b4566f010577a0ee7d9f4"
)

// WatcherClient provide APIs which support others to subscribe to various event from l2geth
type WatcherClient struct {
	ctx context.Context
	event.Feed

	*ethclient.Client

	orm database.OrmFactory

	confirmations       uint64
	proofGenerationFreq uint64
	skippedOpcodes      map[string]struct{}
	messengerAddress    common.Address
	messengerABI        *abi.ABI

	// The height of the block that the watcher has retrieved event logs
	processedMsgHeight uint64

	stopped uint64
	stopCh  chan struct{}

	// mutex for batch proposer
	bpMutex sync.Mutex
}

// NewL2WatcherClient take a l2geth instance to generate a l2watcherclient instance
func NewL2WatcherClient(ctx context.Context, client *ethclient.Client, confirmations uint64, proofGenFreq uint64, skippedOpcodes map[string]struct{}, messengerAddress common.Address, orm database.OrmFactory) *WatcherClient {
	savedHeight, err := orm.GetLayer2LatestWatchedHeight()
	if err != nil {
		log.Warn("fetch height from db failed", "err", err)
		savedHeight = 0
	}

	return &WatcherClient{
		ctx:                 ctx,
		Client:              client,
		orm:                 orm,
		processedMsgHeight:  uint64(savedHeight),
		confirmations:       confirmations,
		proofGenerationFreq: proofGenFreq,
		skippedOpcodes:      skippedOpcodes,
		messengerAddress:    messengerAddress,
		messengerABI:        bridge_abi.L2MessengerMetaABI,
		stopCh:              make(chan struct{}),
		stopped:             0,
		bpMutex:             sync.Mutex{},
	}
}

// Start the Listening process
func (w *WatcherClient) Start() {
	go func() {
		if w.orm == nil {
			panic("must run L2 watcher with DB")
		}

		// trigger by timer
		// TODO: make it configurable
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// get current height
				number, err := w.BlockNumber(w.ctx)
				if err != nil {
					log.Error("failed to get_BlockNumber", "err", err)
					continue
				}
				if err := w.tryFetchRunningMissingBlocks(w.ctx, number); err != nil {
					log.Error("failed to fetchRunningMissingBlocks", "err", err)
				}

				// @todo handle error
				if err := w.fetchContractEvent(number); err != nil {
					log.Error("failed to fetchContractEvent", "err", err)
				}

				if err := w.tryProposeBatch(); err != nil {
					log.Error("failed to tryProposeBatch", "err", err)
				}

			case <-w.stopCh:
				return
			}
		}
	}()
}

// Stop the Watcher module, for a graceful shutdown.
func (w *WatcherClient) Stop() {
	w.stopCh <- struct{}{}
}

const blockTracesFetchLimit = uint64(10)

// try fetch missing blocks if inconsistent
func (w *WatcherClient) tryFetchRunningMissingBlocks(ctx context.Context, backTrackFrom uint64) error {
	// Get newest block in DB. must have blocks at that time.
	// Don't use "block_trace" table "trace" column's BlockTrace.Number,
	// because it might be empty if the corresponding rollup_result is finalized/finalization_skipped
	heightInDB, err := w.orm.GetBlockTracesLatestHeight()
	if err != nil {
		return fmt.Errorf("failed to GetBlockTraces in DB: %v", err)
	}
	backTrackTo := uint64(0)
	if heightInDB > 0 {
		backTrackTo = uint64(heightInDB)
	}

	// note that backTrackFrom >= backTrackTo because we are doing backtracking
	if backTrackFrom > backTrackTo+blockTracesFetchLimit {
		backTrackFrom = backTrackTo + blockTracesFetchLimit
	}

	// start backtracking

	var traces []*types.BlockTrace
	for number := backTrackFrom; number > backTrackTo; number-- {
		log.Debug("retrieving block trace", "height", number)
		trace, err2 := w.GetBlockTraceByNumber(ctx, big.NewInt(int64(number)))
		if err2 != nil {
			return fmt.Errorf("failed to GetBlockResultByHash: %v. number: %v", err2, number)
		}
		log.Info("retrieved block trace", "height", trace.Header.Number, "hash", trace.Header.Hash)

		traces = append(traces, trace)

	}
	if len(traces) > 0 {
		if err = w.orm.InsertBlockTraces(ctx, traces); err != nil {
			return fmt.Errorf("failed to batch insert BlockTraces: %v", err)
		}
	}
	return nil
}

const contractEventsBlocksFetchLimit = int64(10)

// FetchContractEvent pull latest event logs from given contract address and save in DB
func (w *WatcherClient) fetchContractEvent(blockHeight uint64) error {
	fromBlock := int64(w.processedMsgHeight) + 1
	toBlock := int64(blockHeight) - int64(w.confirmations)

	if toBlock < fromBlock {
		return nil
	}

	if toBlock > fromBlock+contractEventsBlocksFetchLimit {
		toBlock = fromBlock + contractEventsBlocksFetchLimit - 1
	}

	// warning: uint int conversion...
	query := ethereum.FilterQuery{
		FromBlock: big.NewInt(fromBlock), // inclusive
		ToBlock:   big.NewInt(toBlock),   // inclusive
		Addresses: []common.Address{
			w.messengerAddress,
		},
		Topics: make([][]common.Hash, 1),
	}
	query.Topics[0] = make([]common.Hash, 1)
	query.Topics[0][0] = common.HexToHash(sentMessageEventSignature)

	logs, err := w.FilterLogs(w.ctx, query)
	if err != nil {
		log.Error("failed to get event logs", "err", err)
		return err
	}
	if len(logs) == 0 {
		return nil
	}
	log.Info("received new L2 messages", "fromBlock", fromBlock, "toBlock", toBlock,
		"cnt", len(logs))

	eventLogs, err := parseBridgeEventLogs(logs, w.messengerABI)
	if err != nil {
		log.Error("failed to parse emitted event log", "err", err)
		return err
	}

	err = w.orm.SaveL2Messages(w.ctx, eventLogs)
	if err == nil {
		w.processedMsgHeight = uint64(toBlock)
	}
	return err
}

func parseBridgeEventLogs(logs []types.Log, messengerABI *abi.ABI) ([]*orm.L2Message, error) {
	// Need use contract abi to parse event Log
	// Can only be tested after we have our contracts set up

	var parsedlogs []*orm.L2Message
	for _, vLog := range logs {
		event := struct {
			Target       common.Address
			Sender       common.Address
			Value        *big.Int // uint256
			Fee          *big.Int // uint256
			Deadline     *big.Int // uint256
			Message      []byte
			MessageNonce *big.Int // uint256
			GasLimit     *big.Int // uint256
		}{}

		err := messengerABI.UnpackIntoInterface(&event, "SentMessage", vLog.Data)
		if err != nil {
			log.Error("failed to unpack layer2 SentMessage event", "err", err)
			return parsedlogs, err
		}
		// target is in topics[1]
		event.Target = common.HexToAddress(vLog.Topics[1].String())
		parsedlogs = append(parsedlogs, &orm.L2Message{
			Nonce:      event.MessageNonce.Uint64(),
			Height:     vLog.BlockNumber,
			Sender:     event.Sender.String(),
			Value:      event.Value.String(),
			Fee:        event.Fee.String(),
			GasLimit:   event.GasLimit.Uint64(),
			Deadline:   event.Deadline.Uint64(),
			Target:     event.Target.String(),
			Calldata:   common.Bytes2Hex(event.Message),
			Layer2Hash: vLog.TxHash.Hex(),
		})
	}

	return parsedlogs, nil
}