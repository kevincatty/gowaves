package miner

import (
	"github.com/wavesplatform/gowaves/pkg/consensus"
	"github.com/wavesplatform/gowaves/pkg/crypto"
	"github.com/wavesplatform/gowaves/pkg/miner/scheduler"
	"github.com/wavesplatform/gowaves/pkg/proto"
	"github.com/wavesplatform/gowaves/pkg/services"
	"github.com/wavesplatform/gowaves/pkg/settings"
	"github.com/wavesplatform/gowaves/pkg/state"
	"github.com/wavesplatform/gowaves/pkg/types"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"bytes"
	"context"
)

type Miner interface {
	Mine(ctx context.Context, t proto.Timestamp, k proto.KeyPair, parent crypto.Signature, baseTarget consensus.BaseTarget, GenSignature crypto.Digest)
}

type DefaultMiner struct {
	utx       types.UtxPool
	state     state.State
	interrupt *atomic.Bool
	services  services.Services
}

func NewDefaultMiner(services services.Services) *DefaultMiner {
	return &DefaultMiner{
		utx:       services.UtxPool,
		state:     services.State,
		interrupt: atomic.NewBool(false),
	}
}

func (a *DefaultMiner) Mine(ctx context.Context, t proto.Timestamp, k proto.KeyPair, parent crypto.Signature, baseTarget consensus.BaseTarget, GenSignature crypto.Digest) {
	a.interrupt.Store(false)
	defer a.services.Scheduler.Reschedule()
	lastKnownBlock, err := a.state.Block(parent)
	if err != nil {
		zap.S().Error(err)
		return
	}

	v, err := blockVersion(a.state)
	if err != nil {
		zap.S().Error(err)
		return
	}

	transactions := proto.Transactions{}
	//var invalidTransactions []*types.TransactionWithBytes
	mu := a.state.Mutex()
	locked := mu.Lock()
	for i := 0; i < 100; i++ {
		tx := a.utx.Pop()
		if tx == nil {
			break
		}

		if a.interrupt.Load() {
			a.state.ResetValidationList()
			locked.Unlock()
			return
		}

		if err = a.state.ValidateNextTx(tx.T, t, lastKnownBlock.Timestamp, v); err == nil {
			transactions = append(transactions, tx.T)
		} // else {
		//invalidTransactions = append(invalidTransactions, t)
		//}
	}
	a.state.ResetValidationList()
	locked.Unlock()

	buf := new(bytes.Buffer)
	_, err = transactions.WriteTo(buf)
	if err != nil {
		return
	}

	nxt := proto.NxtConsensus{
		BaseTarget:   baseTarget,
		GenSignature: GenSignature,
	}

	pub, err := k.Public()
	if err != nil {
		zap.S().Error(err)
		return
	}
	b, err := proto.CreateBlock(proto.NewReprFromTransactions(transactions), t, parent, pub, nxt, v)
	if err != nil {
		zap.S().Error(err)
		return
	}

	priv, err := k.Private()
	if err != nil {
		zap.S().Error(err)
		return
	}

	err = b.Sign(priv)
	if err != nil {
		zap.S().Error(err)
		return
	}

	err = a.services.BlockApplier.Apply(b)
	if err != nil {
		zap.S().Error(err)
	}
}

func (a *DefaultMiner) Interrupt() {
	a.interrupt.Store(true)
}

func Run(ctx context.Context, a Miner, s *scheduler.SchedulerImpl) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-s.Mine():
			a.Mine(ctx, v.Timestamp, v.KeyPair, v.ParentBlockSignature, v.BaseTarget, v.GenSignature)
		}
	}
}

type noOpMiner struct {
}

func (noOpMiner) Interrupt() {
}

func NoOpMiner() noOpMiner {
	return noOpMiner{}
}

func blockVersion(state state.State) (proto.BlockVersion, error) {
	blockRewardActivated, err := state.IsActivated(int16(settings.BlockReward))
	if err != nil {
		return 0, err
	}
	if blockRewardActivated {
		return proto.RewardBlockVersion, nil
	}
	return proto.NgBlockVersion, nil
}
