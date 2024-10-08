package parallel

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/yu-org/yu/core/tripod"

	"github.com/ethereum/go-ethereum/core/state"
	"github.com/yu-org/yu/common"
	"github.com/yu-org/yu/core/context"
	"github.com/yu-org/yu/core/tripod/dev"
	"github.com/yu-org/yu/core/types"

	"github.com/reddio-com/reddio/config"
	"github.com/reddio-com/reddio/evm"
	"github.com/reddio-com/reddio/evm/pending_state"
	"github.com/reddio-com/reddio/metrics"
)

const (
	txnLabelRedoExecute    = "redo"
	txnLabelExecuteSuccess = "success"
	txnLabelErrExecute     = "err"

	batchTxnLabelSuccess = "success"
	batchTxnLabelRedo    = "redo"
)

type ParallelEVM struct {
	*tripod.Tripod
	Solidity *evm.Solidity `tripod:"solidity"`
}

func NewParallelEVM() *ParallelEVM {
	return &ParallelEVM{
		Tripod: tripod.NewTripod(),
	}
}

func (k *ParallelEVM) Execute(block *types.Block) error {
	start := time.Now()
	metrics.BlockExecuteTxnCountGauge.WithLabelValues().Set(float64(len(block.Txns)))
	defer func() {
		metrics.BlockExecuteTxnDuration.WithLabelValues().Observe(time.Since(start).Seconds())
	}()
	txnCtxList, receipts := k.prepareTxnList(block)
	got := k.SplitTxnCtxList(txnCtxList)

	for index, subList := range got {
		k.executeTxnCtxList(subList)
		got[index] = subList
	}
	for _, subList := range got {
		for _, c := range subList {
			receipts[c.txn.TxnHash] = c.receipt
		}
	}
	commitStart := time.Now()
	defer func() {
		metrics.BatchTxnCommitDuration.WithLabelValues().Observe(time.Since(commitStart).Seconds())
	}()
	return k.PostExecute(block, receipts)
}

func (k *ParallelEVM) prepareTxnList(block *types.Block) ([]*txnCtx, map[common.Hash]*types.Receipt) {
	start := time.Now()
	defer func() {
		metrics.BatchTxnPrepareDuration.WithLabelValues().Observe(time.Since(start).Seconds())
	}()
	stxns := block.Txns
	txnCtxList := make([]*txnCtx, len(stxns), len(stxns))
	wg := sync.WaitGroup{}
	for i, subTxn := range stxns {
		wg.Add(1)
		go func(index int, stxn *types.SignedTxn) {
			defer wg.Done()
			stxnCtx := &txnCtx{}
			wrCall := stxn.Raw.WrCall
			ctx, err := context.NewWriteContext(stxn, block, index)
			if err != nil {
				stxnCtx.receipt = k.handleTxnError(err, ctx, block, stxn)
			} else {
				req := &evm.TxRequest{}
				if err := ctx.BindJson(req); err != nil {
					stxnCtx.receipt = k.handleTxnError(err, ctx, block, stxn)
				} else {
					writing, _ := k.Land.GetWriting(wrCall.TripodName, wrCall.FuncName)
					stxnCtx = &txnCtx{
						ctx:     ctx,
						txn:     stxn,
						writing: writing,
						req:     req,
					}
				}
			}
			txnCtxList[index] = stxnCtx
		}(i, subTxn)
	}
	wg.Wait()
	receipts := make(map[common.Hash]*types.Receipt)
	preparedTxnCtxList := make([]*txnCtx, len(stxns), len(stxns))
	successTxnCount := 0
	for _, subTxnCtx := range txnCtxList {
		if subTxnCtx.receipt == nil {
			preparedTxnCtxList[successTxnCount] = subTxnCtx
			successTxnCount++
		} else {
			receipts[subTxnCtx.txn.TxnHash] = subTxnCtx.receipt
		}
	}
	return preparedTxnCtxList[:successTxnCount], receipts
}

func (k *ParallelEVM) SplitTxnCtxList(list []*txnCtx) [][]*txnCtx {
	start := time.Now()
	defer func() {
		metrics.BatchTxnSplitDuration.WithLabelValues().Observe(time.Since(start).Seconds())
	}()
	cur := 0
	curList := make([]*txnCtx, 0)
	got := make([][]*txnCtx, 0)
	for cur < len(list) {
		curTxnCtx := list[cur]
		if checkAddressConflict(curTxnCtx, curList) {
			got = append(got, curList)
			curList = make([]*txnCtx, 0)
			continue
		}
		curList = append(curList, curTxnCtx)
		if len(curList) >= config.GetGlobalConfig().MaxConcurrency {
			got = append(got, curList)
			curList = make([]*txnCtx, 0)
		}
		cur++
	}
	if len(curList) > 0 {
		got = append(got, curList)
	}
	return got
}

func checkAddressConflict(curTxn *txnCtx, curList []*txnCtx) bool {
	for _, compare := range curList {

		if curTxn.req.Address != nil && compare.req.Address != nil {
			if *compare.req.Address == *curTxn.req.Address {
				return true
			}
		}

		if compare.req.Address != nil {
			if *compare.req.Address == curTxn.req.Origin {
				return true
			}
		}

		if curTxn.req.Address != nil {
			if compare.req.Origin == *curTxn.req.Address {
				return true
			}
		}

		if compare.req.Origin == curTxn.req.Origin {
			return true
		}

	}
	return false
}

func (k *ParallelEVM) executeTxnCtxList(list []*txnCtx) []*txnCtx {
	metrics.BatchTxnSplitCounter.WithLabelValues(strconv.FormatInt(int64(len(list)), 10)).Inc()
	if config.GetGlobalConfig().IsParallel {
		return k.executeTxnCtxListInConcurrency(k.Solidity.StateDB(), list)
	}
	return k.executeTxnCtxListInOrder(k.Solidity.StateDB(), list, false)
}

func (k *ParallelEVM) executeTxnCtxListInOrder(originStateDB *state.StateDB, list []*txnCtx, isRedo bool) []*txnCtx {
	currStateDb := originStateDB
	for index, tctx := range list {
		if tctx.err != nil {
			list[index] = tctx
			continue
		}
		tctx.ctx.ExtraInterface = currStateDb
		err := tctx.writing(tctx.ctx)
		if err != nil {
			tctx.err = err
			tctx.receipt = k.handleTxnError(err, tctx.ctx, tctx.ctx.Block, tctx.txn)
		} else {
			tctx.receipt = k.handleTxnEvent(tctx.ctx, tctx.ctx.Block, tctx.txn, isRedo)
			tctx.ps = tctx.ctx.ExtraInterface.(*pending_state.PendingState)
			currStateDb = tctx.ps.GetStateDB()
		}
		list[index] = tctx
	}
	k.Solidity.SetStateDB(currStateDb)
	k.gcCopiedStateDB(nil, list)
	return list
}

func (k *ParallelEVM) executeTxnCtxListInConcurrency(originStateDB *state.StateDB, list []*txnCtx) []*txnCtx {
	conflict := false
	start := time.Now()
	defer func() {
		end := time.Now()
		metrics.BatchTxnDuration.WithLabelValues(fmt.Sprintf("%v", conflict)).Observe(end.Sub(start).Seconds())
	}()
	copiedStateDBList := k.CopyStateDb(originStateDB, list)
	wg := sync.WaitGroup{}
	for i, c := range list {
		wg.Add(1)
		go func(index int, tctx *txnCtx, cpDb *state.StateDB) {
			defer func() {
				wg.Done()
			}()
			tctx.ctx.ExtraInterface = cpDb
			err := tctx.writing(tctx.ctx)
			if err != nil {
				tctx.err = err
				tctx.receipt = k.handleTxnError(err, tctx.ctx, tctx.ctx.Block, tctx.txn)
			} else {
				tctx.receipt = k.handleTxnEvent(tctx.ctx, tctx.ctx.Block, tctx.txn, false)
				tctx.ps = tctx.ctx.ExtraInterface.(*pending_state.PendingState)
			}
			list[index] = tctx
		}(i, c, copiedStateDBList[i])
	}
	wg.Wait()
	curtCtx := pending_state.NewStateContext()
	for _, tctx := range list {
		if tctx.err != nil {
			continue
		}
		if curtCtx.IsConflict(tctx.ps.GetCtx()) {
			conflict = true
			break
		}
	}

	if conflict {
		metrics.BatchTxnCounter.WithLabelValues(batchTxnLabelRedo).Inc()
		return k.executeTxnCtxListInOrder(originStateDB, list, true)
	}
	metrics.BatchTxnCounter.WithLabelValues(batchTxnLabelSuccess).Inc()
	k.mergeStateDB(originStateDB, list)
	k.Solidity.SetStateDB(originStateDB)
	k.gcCopiedStateDB(copiedStateDBList, list)
	return list
}

func (k *ParallelEVM) gcCopiedStateDB(copiedStateDBList []*state.StateDB, list []*txnCtx) {
	copiedStateDBList = nil
	for _, ctx := range list {
		ctx.ctx.ExtraInterface = nil
		ctx.ps = nil
	}
}

func (k *ParallelEVM) mergeStateDB(originStateDB *state.StateDB, list []*txnCtx) {
	k.Solidity.Lock()
	for _, tctx := range list {
		if tctx.err != nil {
			continue
		}
		tctx.ps.MergeInto(originStateDB)
	}
	k.Solidity.Unlock()
}

func (k *ParallelEVM) CopyStateDb(originStateDB *state.StateDB, list []*txnCtx) []*state.StateDB {
	copiedStateDBList := make([]*state.StateDB, 0)
	start := time.Now()
	k.Solidity.Lock()
	defer func() {
		k.Solidity.Unlock()
		metrics.BatchTxnStatedbCopyDuration.WithLabelValues(strconv.FormatInt(int64(len(list)), 10)).Observe(time.Now().Sub(start).Seconds())
	}()
	for i := 0; i < len(list); i++ {
		copiedStateDBList = append(copiedStateDBList, originStateDB.Copy())
	}
	return copiedStateDBList
}

type txnCtx struct {
	ctx     *context.WriteContext
	txn     *types.SignedTxn
	writing dev.Writing
	req     *evm.TxRequest
	err     error
	ps      *pending_state.PendingState
	receipt *types.Receipt
}

func (k *ParallelEVM) handleTxnError(err error, ctx *context.WriteContext, block *types.Block, stxn *types.SignedTxn) *types.Receipt {
	metrics.TxnCounter.WithLabelValues(txnLabelErrExecute).Inc()
	return k.HandleError(err, ctx, block, stxn)
}

func (k *ParallelEVM) handleTxnEvent(ctx *context.WriteContext, block *types.Block, stxn *types.SignedTxn, isRedo bool) *types.Receipt {
	metrics.TxnCounter.WithLabelValues(txnLabelExecuteSuccess).Inc()
	if isRedo {
		metrics.TxnCounter.WithLabelValues(txnLabelRedoExecute).Inc()
	}
	return k.HandleEvent(ctx, block, stxn)
}
