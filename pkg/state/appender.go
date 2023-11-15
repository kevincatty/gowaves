package state

import (
	"fmt"

	"github.com/mr-tron/base58/base58"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/wavesplatform/gowaves/pkg/crypto"
	"github.com/wavesplatform/gowaves/pkg/errs"
	"github.com/wavesplatform/gowaves/pkg/proto"
	"github.com/wavesplatform/gowaves/pkg/settings"
	"github.com/wavesplatform/gowaves/pkg/types"
)

type blockInfoProvider interface {
	NewestBlockInfoByHeight(height proto.Height) (*proto.BlockInfo, error)
}

type txAppender struct {
	sc                *scriptCaller
	ia                *invokeApplier
	ethTxKindResolver proto.EthereumTransactionKindResolver
	rw                *blockReadWriter

	blockInfoProvider blockInfoProvider

	atx      *addressTransactions
	stor     *blockchainEntitiesStorage
	settings *settings.BlockchainSettings

	// TransactionHandler is handler for any operations on transactions.
	txHandler *transactionHandler
	// Block differ is used to create diffs from blocks.
	blockDiffer *blockDiffer
	// Storage for diffs of incoming transactions (from added blocks or UTX).
	// It will be used for validation and applying diffs to existing balances.
	diffStor *diffStorage
	// diffStorInvoke is storage for partial diffs generated by Invoke transactions.
	// It is used to calculate balances that take into account intermediate invoke changes for RIDE.
	diffStorInvoke *diffStorageWrapped
	// Ids of all transactions whose diffs are currently in diffStor.
	// This is needed to check that transaction ids are unique.
	recentTxIds map[string]struct{}
	// diffApplier is used to both validate and apply balance diffs.
	diffApplier *diffApplier

	// totalScriptsRuns counts script runs in block / UTX.
	totalScriptsRuns uint64

	// buildApiData flag indicates that additional data for API is built when
	// appending transactions.
	buildApiData bool
}

func newTxAppender(
	state types.SmartState,
	rw *blockReadWriter,
	stor *blockchainEntitiesStorage,
	settings *settings.BlockchainSettings,
	stateDB *stateDB,
	atx *addressTransactions,
	snapshotApplier *blockSnapshotsApplier,
	snapshotGenerator *snapshotGenerator,
) (*txAppender, error) {
	sc, err := newScriptCaller(state, stor, settings)
	if err != nil {
		return nil, err
	}
	genesis := settings.Genesis
	txHandler, err := newTransactionHandler(genesis.BlockID(), stor, settings, snapshotGenerator, snapshotApplier)
	if err != nil {
		return nil, err
	}
	blockDiffer, err := newBlockDiffer(txHandler, stor, settings)
	if err != nil {
		return nil, err
	}
	diffStor, err := newDiffStorage()
	if err != nil {
		return nil, err
	}
	diffStorInvoke, err := newDiffStorageWrapped(diffStor)
	if err != nil {
		return nil, err
	}
	diffApplier, err := newDiffApplier(stor.balances, settings.AddressSchemeCharacter)
	if err != nil {
		return nil, err
	}
	buildApiData, err := stateDB.stateStoresApiData()
	if err != nil {
		return nil, err
	}
	ia := newInvokeApplier(state, sc, txHandler, stor, settings, blockDiffer, diffStorInvoke, diffApplier, buildApiData)
	ethKindResolver := proto.NewEthereumTransactionKindResolver(state, settings.AddressSchemeCharacter)
	return &txAppender{
		sc:                sc,
		ia:                ia,
		rw:                rw,
		blockInfoProvider: state,
		atx:               atx,
		stor:              stor,
		settings:          settings,
		txHandler:         txHandler,
		blockDiffer:       blockDiffer,
		recentTxIds:       make(map[string]struct{}),
		diffStor:          diffStor,
		diffStorInvoke:    diffStorInvoke,
		diffApplier:       diffApplier,
		buildApiData:      buildApiData,
		ethTxKindResolver: ethKindResolver,
	}, nil
}

func (a *txAppender) checkDuplicateTxIdsImpl(id []byte, recentIds map[string]struct{}) error {
	// Check recent.
	if _, ok := recentIds[string(id)]; ok {
		return proto.NewInfoMsg(errors.Errorf("transaction with ID %s already in state", base58.Encode(id)))
	}
	// Check DB.
	if _, _, err := a.rw.readTransaction(id); err == nil {
		return proto.NewInfoMsg(errors.Errorf("transaction with ID %s already in state", base58.Encode(id)))
	}
	return nil
}

func (a *txAppender) checkDuplicateTxIds(tx proto.Transaction, recentIds map[string]struct{}, timestamp uint64) error {
	if tx.GetTypeInfo().Type == proto.PaymentTransaction {
		// Payment transactions are deprecated.
		return nil
	}
	if tx.GetTypeInfo().Type == proto.CreateAliasTransaction {
		if (timestamp >= a.settings.StolenAliasesWindowTimeStart) && (timestamp <= a.settings.StolenAliasesWindowTimeEnd) {
			// At this period alias transactions might have duplicate IDs due to bugs in historical blockchain.
			return nil
		}
	}
	txID, err := tx.GetID(a.settings.AddressSchemeCharacter)
	if err != nil {
		return err
	}
	err = a.checkDuplicateTxIdsImpl(txID, recentIds)
	if err != nil {
		if tx.GetTypeInfo().Type == proto.CreateAliasTransaction {
			return errs.NewAliasTaken(err.Error())
		}
	}
	return err
}

type appendBlockParams struct {
	transactions  []proto.Transaction
	chans         *verifierChans
	block, parent *proto.BlockHeader
	height        uint64
}

func (a *txAppender) orderIsScripted(order proto.Order) (bool, error) {
	return a.txHandler.tc.orderScriptedAccount(order)
}

// For UTX validation, this returns the last stable block, which is in fact current block.
func (a *txAppender) currentBlock() (*proto.BlockHeader, error) {
	curBlockHeight := a.rw.addingBlockHeight()
	curHeader, err := a.rw.readNewestBlockHeaderByHeight(curBlockHeight)
	if err != nil {
		return nil, err
	}
	return curHeader, nil
}

func (a *txAppender) currentBlockInfo() (*proto.BlockInfo, error) {
	curBlockHeight := a.rw.addingBlockHeight()
	return a.blockInfoProvider.NewestBlockInfoByHeight(curBlockHeight)
}

func (a *txAppender) checkProtobufVersion(tx proto.Transaction, blockV5Activated bool) error {
	if !proto.IsProtobufTx(tx) {
		return nil
	}
	if !blockV5Activated {
		return errors.Errorf("bad transaction version %v before blockV5 activation", tx.GetVersion())
	}
	return nil
}

func (a *txAppender) checkTxFees(tx proto.Transaction, info *fallibleValidationParams) error {
	var (
		feeChanges txBalanceChanges
		err        error
	)
	di := newDifferInfo(info.blockInfo)
	switch tx.GetTypeInfo().Type {
	case proto.ExchangeTransaction:
		feeChanges, err = a.txHandler.td.createDiffForExchangeFeeValidation(tx, di)
		if err != nil {
			return err
		}
	case proto.InvokeScriptTransaction:
		feeChanges, err = a.txHandler.td.createFeeDiffInvokeScriptWithProofs(tx, di)
		if err != nil {
			return err
		}
	case proto.InvokeExpressionTransaction:
		feeChanges, err = a.txHandler.td.createFeeDiffInvokeExpressionWithProofs(tx, di)
		if err != nil {
			return err
		}
		// TODO handle ethereum invoke expression tx
	case proto.EthereumMetamaskTransaction:
		feeChanges, err = a.txHandler.td.createFeeDiffEthereumInvokeScriptWithProofs(tx, di)
		if err != nil {
			return err
		}
	default:
		return errors.Errorf("failed to check tx fees: wrong tx type=%d (%T)", tx.GetTypeInfo().Type, tx)
	}

	return a.diffApplier.validateTxDiff(feeChanges.diff, a.diffStor)
}

// This function is used for script validation of transaction that can't fail.
func (a *txAppender) checkTransactionScripts(tx proto.Transaction, accountScripted bool, params *appendTxParams) (uint64, txCheckerData, error) {
	scriptsRuns := uint64(0)
	if accountScripted {
		// Check script.
		if err := a.sc.callAccountScriptWithTx(tx, params); err != nil {
			return 0, txCheckerData{}, errs.Extend(err, "callAccountScriptWithTx")
		}
		scriptsRuns++
	}
	// Check against state.
	checkerData, err := a.txHandler.checkTx(tx, params.checkerInfo)
	if err != nil {
		return 0, txCheckerData{}, err
	}
	txSmartAssets := checkerData.smartAssets

	ride4DAppsActivated, err := a.stor.features.newestIsActivated(int16(settings.Ride4DApps))
	if err != nil {
		return 0, txCheckerData{}, errs.Extend(err, "isActivated")
	}
	for _, smartAsset := range txSmartAssets {
		// Check smart asset's script.
		r, err := a.sc.callAssetScript(tx, smartAsset, params)
		if err != nil {
			return 0, txCheckerData{}, errs.Extend(err, "callAssetScript")
		}
		if !r.Result() {
			return 0, txCheckerData{}, errs.Extend(errors.New("negative asset script result"), "callAssetScript")
		}
		if tx.GetTypeInfo().Type == proto.SetAssetScriptTransaction && !ride4DAppsActivated {
			// Exception: don't count before Ride4DApps activation.
			continue
		}
		scriptsRuns++
	}
	return scriptsRuns, checkerData, nil
}

func (a *txAppender) checkScriptsLimits(scriptsRuns uint64, blockID proto.BlockID) error {
	smartAccountsActivated, err := a.stor.features.newestIsActivated(int16(settings.SmartAccounts))
	if err != nil {
		return err
	}
	ride4DAppsActivated, err := a.stor.features.newestIsActivated(int16(settings.Ride4DApps))
	if err != nil {
		return err
	}
	if ride4DAppsActivated {
		rideV5Activated, err := a.stor.features.newestIsActivated(int16(settings.RideV5))
		if err != nil {
			return errors.Wrapf(err, "failed to check if feature %d is activated", settings.RideV5)
		}
		maxBlockComplexity := NewMaxScriptsComplexityInBlock().GetMaxScriptsComplexityInBlock(rideV5Activated)
		if a.sc.getTotalComplexity() > uint64(maxBlockComplexity) {
			rideV6Activated, err := a.stor.features.newestIsActivated(int16(settings.RideV6))
			if err != nil {
				return errors.Wrapf(err, "failed to check if feature %d is activated", settings.RideV6)
			}
			if rideV6Activated {
				return errors.Errorf("complexity of scripts (%d) in block '%s' exceeds limit of %d",
					a.sc.getTotalComplexity(), blockID.String(), maxBlockComplexity,
				)
			}
			zap.S().Warnf("Complexity of scripts (%d) in block '%s' exceeds limit of %d",
				a.sc.getTotalComplexity(), blockID.String(), maxBlockComplexity,
			)
		}
		return nil
	} else if smartAccountsActivated {
		if scriptsRuns > maxScriptsRunsInBlock {
			return errors.Errorf("more scripts runs in block than allowed: %d > %d", scriptsRuns, maxScriptsRunsInBlock)
		}
	}
	return nil
}

func (a *txAppender) needToCheckOrdersSignatures(transaction proto.Transaction) (bool, bool, error) {
	tx, ok := transaction.(proto.Exchange)
	if !ok {
		return false, false, nil
	}
	o1Scripted, err := a.orderIsScripted(tx.GetOrder1())
	if err != nil {
		return false, false, err
	}
	o2Scripted, err := a.orderIsScripted(tx.GetOrder2())
	if err != nil {
		return false, false, err
	}
	return !o1Scripted, !o2Scripted, nil
}

func (a *txAppender) saveTransactionIdByAddresses(addresses []proto.WavesAddress, txID []byte, blockID proto.BlockID) error {
	for _, addr := range addresses {
		if err := a.atx.saveTxIdByAddress(addr, txID, blockID); err != nil {
			return err
		}
	}
	return nil
}

func (a *txAppender) commitTxApplication(
	tx proto.Transaction,
	params *appendTxParams,
	invocationRes *invocationResult,
	applicationRes *applicationResult) (txSnapshot, error) {
	// Add transaction ID to recent IDs.
	txID, err := tx.GetID(a.settings.AddressSchemeCharacter)
	if err != nil {
		return txSnapshot{}, wrapErr(TxCommitmentError, errors.Errorf("failed to get tx id: %v", err))
	}
	a.recentTxIds[string(txID)] = empty
	// Update script runs.
	a.totalScriptsRuns += applicationRes.totalScriptsRuns
	// Update complexity.
	a.sc.addRecentTxComplexity()
	// Save balance diff.
	if err = a.diffStor.saveTxDiff(applicationRes.changes.diff); err != nil {
		return txSnapshot{}, wrapErr(TxCommitmentError, errors.Errorf("failed to save balance diff: %v", err))
	}
	currentMinerAddress := proto.MustAddressFromPublicKey(a.settings.AddressSchemeCharacter, params.currentMinerPK)

	var snapshot txSnapshot
	if applicationRes.status {
		// We only perform tx in case it has not failed.
		performerInfo := &performerInfo{
			height:              params.checkerInfo.height,
			blockID:             params.checkerInfo.blockID,
			currentMinerAddress: currentMinerAddress,
			checkerData:         applicationRes.checkerData,
			stateActionsCounter: params.stateActionsCounterInBlock,
		}
		snapshot, err = a.txHandler.performTx(tx, performerInfo, invocationRes, applicationRes.changes.diff)
		if err != nil {
			return txSnapshot{}, wrapErr(TxCommitmentError, errors.Errorf("failed to perform: %v", err))
		}
	}
	if params.validatingUtx {
		// Save transaction to in-mem storage.
		if err = a.rw.writeTransactionToMem(tx, !applicationRes.status); err != nil {
			return txSnapshot{}, wrapErr(TxCommitmentError,
				errors.Errorf("failed to write transaction to in mem stor: %v", err),
			)
		}
	} else {
		// Count tx fee.
		if err := a.blockDiffer.countMinerFee(tx); err != nil {
			return txSnapshot{}, wrapErr(TxCommitmentError, errors.Errorf("failed to count miner fee: %v", err))
		}
		// Save transaction to storage.
		if err = a.rw.writeTransaction(tx, !applicationRes.status); err != nil {
			return txSnapshot{}, wrapErr(TxCommitmentError,
				errors.Errorf("failed to write transaction to storage: %v", err),
			)
		}
	}
	// TODO: transaction status snapshot has to be appended here
	return snapshot, nil
}

func (a *txAppender) verifyWavesTxSigAndData(tx proto.Transaction, params *appendTxParams, accountHasVerifierScript bool) error {
	// Detect what signatures must be checked for this transaction.
	// For transaction with SmartAccount we don't check signature.
	checkTxSig := !accountHasVerifierScript
	checkOrder1, checkOrder2, err := a.needToCheckOrdersSignatures(tx)
	if err != nil {
		return err
	}
	if checkSequentially := params.validatingUtx; checkSequentially {
		// In UTX it is not very useful to check signatures in separate goroutines,
		// because they have to be checked in each validateNextTx() anyway.
		return checkTx(tx, checkTxSig, checkOrder1, checkOrder2, a.settings.AddressSchemeCharacter)
	}
	// Send transaction for validation of transaction's data correctness (using tx.Validate() method)
	// and simple cryptographic signature verification (using tx.Verify() and PK).
	task := &verifyTask{
		taskType:    verifyTx,
		tx:          tx,
		checkTxSig:  checkTxSig,
		checkOrder1: checkOrder1,
		checkOrder2: checkOrder2,
	}
	return params.chans.trySend(task)
}

// appendTxParams contains params which are necessary for tx or block appending
// TODO: create features provider instead of passing new params
type appendTxParams struct {
	chans                            *verifierChans // can be nil if validatingUtx == true
	checkerInfo                      *checkerInfo
	blockInfo                        *proto.BlockInfo
	block                            *proto.BlockHeader
	acceptFailed                     bool
	blockV5Activated                 bool
	rideV5Activated                  bool
	rideV6Activated                  bool
	consensusImprovementsActivated   bool
	blockRewardDistributionActivated bool
	invokeExpressionActivated        bool // TODO: check feature naming
	validatingUtx                    bool // if validatingUtx == false then chans MUST be initialized with non nil value
	stateActionsCounterInBlock       *proto.StateActionsCounter
	currentMinerPK                   crypto.PublicKey
}

func (a *txAppender) handleInvokeOrExchangeTransaction(
	tx proto.Transaction,
	fallibleInfo *fallibleValidationParams) (*invocationResult, *applicationResult, error) {
	invocationRes, applicationRes, err := a.handleFallible(tx, fallibleInfo)
	if err != nil {
		msg := "fallible validation failed"
		if txID, err2 := tx.GetID(a.settings.AddressSchemeCharacter); err2 == nil {
			msg = fmt.Sprintf("fallible validation failed for transaction '%s'", base58.Encode(txID))
		}
		return nil, nil, errs.Extend(err, msg)
	}
	return invocationRes, applicationRes, nil
}

func (a *txAppender) handleDefaultTransaction(tx proto.Transaction, params *appendTxParams, accountHasVerifierScript bool) (*applicationResult, error) {
	// Execute transaction's scripts, check against state.
	txScriptsRuns, checkerData, err := a.checkTransactionScripts(tx, accountHasVerifierScript, params)
	if err != nil {
		return nil, err
	}
	// Create balance diff of this tx.
	txChanges, err := a.blockDiffer.createTransactionDiff(tx, params.block, newDifferInfo(params.blockInfo))
	if err != nil {
		return nil, errs.Extend(err, "create transaction diff")
	}
	return newApplicationResult(true, txScriptsRuns, txChanges, checkerData), nil
}

func (a *txAppender) appendTx(tx proto.Transaction, params *appendTxParams) error {
	defer func() {
		a.sc.resetRecentTxComplexity()
		a.stor.dropUncertain()
	}()

	blockID := params.checkerInfo.blockID
	// Check that Protobuf transactions are accepted.
	if err := a.checkProtobufVersion(tx, params.blockV5Activated); err != nil {
		return err
	}
	// Check transaction for duplication of its ID.
	if err := a.checkDuplicateTxIds(tx, a.recentTxIds, params.block.Timestamp); err != nil {
		return errs.Extend(err, "check duplicate tx ids")
	}
	// Verify tx signature and internal data correctness.
	senderAddr, err := tx.GetSender(a.settings.AddressSchemeCharacter)
	if err != nil {
		return errs.Extend(err, "failed to get sender addr by pk")
	}

	// senderWavesAddr needs only for newestAccountHasVerifier check
	senderWavesAddr, err := senderAddr.ToWavesAddress(a.settings.AddressSchemeCharacter)
	if err != nil {
		return errors.Wrapf(err, "failed to transform (%T) address type to WavesAddress type", senderAddr)
	}
	accountHasVerifierScript, err := a.stor.scriptsStorage.newestAccountHasVerifier(senderWavesAddr)
	if err != nil {
		return errs.Extend(err, "account has verifier")
	}

	if err := a.verifyWavesTxSigAndData(tx, params, accountHasVerifierScript); err != nil {
		return errs.Extend(err, "tx signature or data verification failed")
	}

	// Check tx against state, check tx scripts, calculate balance changes.
	var applicationRes *applicationResult
	var invocationResult *invocationResult
	needToValidateBalanceDiff := false
	switch tx.GetTypeInfo().Type {
	case proto.InvokeScriptTransaction, proto.InvokeExpressionTransaction, proto.ExchangeTransaction:
		// Invoke and Exchange transactions should be handled differently.
		// They may fail, and will be saved to blockchain anyway.
		fallibleInfo := &fallibleValidationParams{appendTxParams: params, senderScripted: accountHasVerifierScript, senderAddress: senderAddr}
		invocationResult, applicationRes, err = a.handleInvokeOrExchangeTransaction(tx, fallibleInfo)
		if err != nil {
			return errors.Wrap(err, "failed to handle invoke or exchange transaction")
		}
		// Exchange and Invoke balances are validated in UTX when acceptFailed is false.
		// When acceptFailed is true, balances are validated inside handleFallible().
		needToValidateBalanceDiff = params.validatingUtx && !params.acceptFailed
	case proto.EthereumMetamaskTransaction:
		ethTx, ok := tx.(*proto.EthereumTransaction)
		if !ok {
			return errors.New("failed to cast interface transaction to ethereum transaction structure")
		}
		kind, err := a.ethTxKindResolver.ResolveTxKind(ethTx, params.blockRewardDistributionActivated)
		if err != nil {
			return errors.Wrap(err, "failed to guess ethereum transaction kind")
		}
		ethTx.TxKind = kind

		switch ethTx.TxKind.(type) {
		case *proto.EthereumTransferWavesTxKind, *proto.EthereumTransferAssetsErc20TxKind:
			applicationRes, err = a.handleDefaultTransaction(tx, params, accountHasVerifierScript)
			if err != nil {
				return errors.Wrapf(err, "failed to handle ethereum transaction (type %s) with id %s, on height %d",
					ethTx.TxKind.String(), ethTx.ID.String(), params.checkerInfo.height+1)
			}
			// In UTX balances are always validated.
			needToValidateBalanceDiff = params.validatingUtx
		case *proto.EthereumInvokeScriptTxKind:
			fallibleInfo := &fallibleValidationParams{
				appendTxParams: params,
				senderScripted: accountHasVerifierScript,
				senderAddress:  senderAddr,
			}
			invocationResult, applicationRes, err = a.handleInvokeOrExchangeTransaction(tx, fallibleInfo)
			if err != nil {
				return errors.Wrapf(err, "failed to handle ethereum invoke script transaction (type %s) with id %s, on height %d",
					ethTx.TxKind.String(), ethTx.ID.String(), params.checkerInfo.height+1)
			}
		}
	default:
		applicationRes, err = a.handleDefaultTransaction(tx, params, accountHasVerifierScript)
		if err != nil {
			id, idErr := tx.GetID(a.settings.AddressSchemeCharacter)
			if idErr != nil {
				return errors.Wrap(err, "failed to generate transaction ID")
			}
			return errors.Wrapf(err, "failed to handle transaction '%s'", base58.Encode(id))
		}
		// In UTX balances are always validated.
		needToValidateBalanceDiff = params.validatingUtx
	}
	if needToValidateBalanceDiff {
		// Validate balance diff for negative balances.
		if err := a.diffApplier.validateTxDiff(applicationRes.changes.diff, a.diffStor); err != nil {
			return errs.Extend(err, "validate transaction diff")
		}
	}
	// Check complexity limits and scripts runs limits.
	if err := a.checkScriptsLimits(a.totalScriptsRuns+applicationRes.totalScriptsRuns, blockID); err != nil {
		return errs.Extend(errors.Errorf("%s: %v", blockID.String(), err), "check scripts limits")
	}
	// Perform state changes, save balance changes, write tx to storage.
	txID, err := tx.GetID(a.settings.AddressSchemeCharacter)
	if err != nil {
		return errs.Extend(err, "get transaction id")
	}

	// invocationResult may be empty if it was not an Invoke Transaction
	_, err = a.commitTxApplication(tx, params, invocationResult, applicationRes)
	if err != nil {
		zap.S().Errorf("failed to commit transaction (id %s) after successful validation; this should NEVER happen", base58.Encode(txID))
		return err
	}
	// Store additional data for API: transaction by address.
	if !params.validatingUtx && a.buildApiData {
		if err := a.saveTransactionIdByAddresses(applicationRes.changes.addresses(), txID, blockID); err != nil {
			return errs.Extend(err, "save transaction id by addresses")
		}
	}
	return nil
}

// rewards and 60% of the fee to the previous miner.
func (a *txAppender) createInitialBlockSnapshot(minerAndRewardDiff txDiff) (txSnapshot, error) {
	addrWavesBalanceDiff, _, err := balanceDiffFromTxDiff(minerAndRewardDiff, a.settings.AddressSchemeCharacter)
	if err != nil {
		return txSnapshot{}, errors.Wrap(err, "failed to create balance diff from tx diff")
	}
	// add miner address to the diff
	var snapshot txSnapshot
	for wavesAddress, diffAmount := range addrWavesBalanceDiff {
		var fullBalance balanceProfile
		fullBalance, err = a.stor.balances.newestWavesBalance(wavesAddress.ID())
		if err != nil {
			return txSnapshot{}, errors.Wrap(err, "failed to receive sender's waves balance")
		}
		newBalance := &proto.WavesBalanceSnapshot{
			Address: wavesAddress,
			Balance: uint64(int64(fullBalance.balance) + diffAmount.balance),
		}
		snapshot.regular = append(snapshot.regular, newBalance)
	}
	return snapshot, nil
}

func (a *txAppender) appendBlock(params *appendBlockParams) error {
	// Reset block complexity counter.
	defer func() {
		a.sc.resetComplexity()
		a.totalScriptsRuns = 0
	}()
	rideV5Activated, err := a.stor.features.newestIsActivated(int16(settings.RideV5))
	if err != nil {
		return err
	}
	rideV6Activated, err := a.stor.features.newestIsActivated(int16(settings.RideV6))
	if err != nil {
		return err
	}
	blockRewardDistribution, err := a.stor.features.newestIsActivated(int16(settings.BlockRewardDistribution))
	if err != nil {
		return err
	}
	checkerInfo := &checkerInfo{
		currentTimestamp:        params.block.Timestamp,
		blockID:                 params.block.BlockID(),
		blockVersion:            params.block.Version,
		height:                  params.height,
		rideV5Activated:         rideV5Activated,
		rideV6Activated:         rideV6Activated,
		blockRewardDistribution: blockRewardDistribution,
	}
	hasParent := params.parent != nil
	if hasParent {
		checkerInfo.parentTimestamp = params.parent.Timestamp
	}
	stateActionsCounterInBlockValidation := new(proto.StateActionsCounter)

	snapshotApplierInfo := newBlockSnapshotsApplierInfo(checkerInfo, a.settings.AddressSchemeCharacter,
		stateActionsCounterInBlockValidation)
	a.txHandler.tp.snapshotApplier.SetApplierInfo(snapshotApplierInfo)
	// Create miner balance diff.
	// This adds 60% of prev block fees as very first balance diff of the current block
	// in case NG is activated, or empty diff otherwise.
	minerAndRewardDiff, err := a.blockDiffer.createMinerAndRewardDiff(params.block, hasParent)
	if err != nil {
		return err
	}
	// create the initial snapshot
	_, err = a.createInitialBlockSnapshot(minerAndRewardDiff)
	if err != nil {
		return errors.Wrap(err, "failed to create initial snapshot")
	}

	// TODO apply this snapshot when balances are refatored
	// err = initialSnapshot.Apply(&snapshotApplier)

	// Save miner diff first (for validation)
	if err = a.diffStor.saveTxDiff(minerAndRewardDiff); err != nil {
		return err
	}
	blockInfo, err := a.currentBlockInfo()
	if err != nil {
		return err
	}
	blockV5Activated, err := a.stor.features.newestIsActivated(int16(settings.BlockV5))
	if err != nil {
		return err
	}
	consensusImprovementsActivated, err := a.stor.features.newestIsActivated(int16(settings.ConsensusImprovements))
	if err != nil {
		return err
	}
	blockRewardDistributionActivated, err := a.stor.features.newestIsActivated(int16(settings.BlockRewardDistribution))
	if err != nil {
		return err
	}
	invokeExpressionActivated, err := a.stor.features.newestIsActivated(int16(settings.InvokeExpression))
	if err != nil {
		return err
	}
	// Check and append transactions.

	for _, tx := range params.transactions {
		appendTxArgs := &appendTxParams{
			chans:                            params.chans,
			checkerInfo:                      checkerInfo,
			blockInfo:                        blockInfo,
			block:                            params.block,
			acceptFailed:                     blockV5Activated,
			blockV5Activated:                 blockV5Activated,
			rideV5Activated:                  rideV5Activated,
			rideV6Activated:                  rideV6Activated,
			consensusImprovementsActivated:   consensusImprovementsActivated,
			blockRewardDistributionActivated: blockRewardDistributionActivated,
			invokeExpressionActivated:        invokeExpressionActivated,
			validatingUtx:                    false,
			stateActionsCounterInBlock:       stateActionsCounterInBlockValidation,
			currentMinerPK:                   params.block.GeneratorPublicKey,
		}
		if err := a.appendTx(tx, appendTxArgs); err != nil {
			return err
		}
	}
	// Save fee distribution of this block.
	// This will be needed for createMinerAndRewardDiff() of next block due to NG.
	if err := a.blockDiffer.saveCurFeeDistr(params.block); err != nil {
		return err
	}
	return nil
}

// used only in tests now. All diffs are applied in snapshotApplier.
func (a *txAppender) applyAllDiffs() error {
	a.recentTxIds = make(map[string]struct{})
	return a.moveChangesToHistoryStorage()
}

func (a *txAppender) moveChangesToHistoryStorage() error {
	changes := a.diffStor.allChanges()
	a.diffStor.reset()
	return a.diffApplier.applyBalancesChanges(changes)
}

type fallibleValidationParams struct {
	*appendTxParams
	senderScripted bool
	senderAddress  proto.Address
}

type applicationResult struct {
	status           bool
	totalScriptsRuns uint64
	changes          txBalanceChanges
	checkerData      txCheckerData
}

func newApplicationResult(status bool, totalScriptsRuns uint64, changes txBalanceChanges, checkerData txCheckerData) *applicationResult {
	return &applicationResult{status, totalScriptsRuns, changes, checkerData} // all fields must be initialized
}

func (a *txAppender) handleInvoke(
	tx proto.Transaction,
	info *fallibleValidationParams) (*invocationResult, *applicationResult, error) {
	var ID crypto.Digest
	switch t := tx.(type) {
	case *proto.InvokeScriptWithProofs:
		ID = *t.ID
	case *proto.InvokeExpressionTransactionWithProofs:
		ID = *t.ID
	case *proto.EthereumTransaction:
		switch t.TxKind.(type) {
		case *proto.EthereumInvokeScriptTxKind:
			ID = *t.ID
		default:
			return nil, nil, errors.Errorf("unexpected ethereum tx kind (%T)", tx)
		}
	default:
		return nil, nil, errors.Errorf("failed to handle invoke: wrong type of transaction (%T)", tx)
	}
	invocationRes, applicationRes, err := a.ia.applyInvokeScript(tx, info)
	if err != nil {
		zap.S().Debugf("failed to apply InvokeScript transaction %s to state: %v", ID.String(), err)
		return nil, nil, err
	}
	return invocationRes, applicationRes, nil
}

func (a *txAppender) countExchangeScriptsRuns(scriptsRuns uint64) (uint64, error) {
	// Some bug in historical blockchain, no logic here.
	ride4DAppsActivated, err := a.stor.features.newestIsActivated(int16(settings.Ride4DApps))
	if err != nil {
		return 0, err
	}
	if !ride4DAppsActivated {
		// Don't count before Ride4DApps activation.
		return 0, nil
	}
	return scriptsRuns, nil
}

func (a *txAppender) handleExchange(tx proto.Transaction, info *fallibleValidationParams) (*applicationResult, error) {
	exchange, ok := tx.(proto.Exchange)
	if !ok {
		return nil, errors.New("failed to convert transaction to Exchange")
	}
	// If BlockV5 feature is not activated, we never accept failed transactions.
	info.acceptFailed = info.blockV5Activated && info.acceptFailed
	scriptsRuns := uint64(0)
	// At first, we call accounts and orders scripts which must not fail.
	if info.senderScripted {
		// Check script on account.
		err := a.sc.callAccountScriptWithTx(tx, info.appendTxParams)
		if err != nil {
			return nil, err
		}
		scriptsRuns++
	}
	// Smart account trading.
	smartAccountTradingActivated, err := a.stor.features.newestIsActivated(int16(settings.SmartAccountTrading))
	if err != nil {
		return nil, err
	}
	if smartAccountTradingActivated {
		// Check orders scripts.
		o1 := exchange.GetOrder1()
		o2 := exchange.GetOrder2()
		o1Scripted, err := a.orderIsScripted(o1)
		if err != nil {
			return nil, err
		}
		o2Scripted, err := a.orderIsScripted(o2)
		if err != nil {
			return nil, err
		}
		if o1Scripted {
			if err := a.sc.callAccountScriptWithOrder(o1, info.blockInfo, info); err != nil {
				return nil, errors.Wrap(err, "script failure on first order")
			}
			scriptsRuns++
		}
		if o2Scripted {
			if err := a.sc.callAccountScriptWithOrder(o2, info.blockInfo, info); err != nil {
				return nil, errors.Wrap(err, "script failure on second order")
			}
			scriptsRuns++
		}
	}
	// Validate transaction, orders and extract smart assets.
	checkerData, err := a.txHandler.checkTx(tx, info.checkerInfo)
	if err != nil {
		return nil, err
	}
	txSmartAssets := checkerData.smartAssets

	// Count total scripts runs.
	scriptsRuns += uint64(len(txSmartAssets))
	scriptsRuns, err = a.countExchangeScriptsRuns(scriptsRuns)
	if err != nil {
		return nil, err
	}
	// Create balance changes for both failure and success.
	di := newDifferInfo(info.blockInfo)
	failedChanges, err := a.blockDiffer.createFailedTransactionDiff(tx, info.block, di)
	if err != nil {
		return nil, err
	}
	successfulChanges, err := a.blockDiffer.createTransactionDiff(tx, info.block, di)
	if err != nil {
		return nil, err
	}
	// Check smart assets' scripts.
	for _, smartAsset := range txSmartAssets {
		res, err := a.sc.callAssetScript(tx, smartAsset, info.appendTxParams)
		if err != nil && !info.acceptFailed {
			return nil, err
		}
		if err != nil || !res.Result() {
			// Smart asset script failed, return failed diff.
			return newApplicationResult(false, scriptsRuns, failedChanges, checkerData), nil
		}
	}
	if info.acceptFailed {
		// If accepting failed, we must also check resulting balances.
		if err := a.diffApplier.validateTxDiff(successfulChanges.diff, a.diffStor); err != nil {
			// Not enough balance for successful diff = fail, return failed diff.
			// We only check successful diff for negative balances, because failed diff is already checked in checkTxFees().
			return newApplicationResult(false, scriptsRuns, failedChanges, checkerData), nil
		}
	}
	// Return successful diff.
	return newApplicationResult(true, scriptsRuns, successfulChanges, checkerData), nil
}

func (a *txAppender) handleFallible(
	tx proto.Transaction,
	info *fallibleValidationParams) (*invocationResult, *applicationResult, error) {
	if info.acceptFailed {
		if err := a.checkTxFees(tx, info); err != nil {
			return nil, nil, err
		}
	}
	switch tx.GetTypeInfo().Type {
	case proto.InvokeScriptTransaction, proto.InvokeExpressionTransaction, proto.EthereumMetamaskTransaction:
		return a.handleInvoke(tx, info)
	case proto.ExchangeTransaction:
		applicationRes, err := a.handleExchange(tx, info)
		return nil, applicationRes, err
	}
	return nil, nil, errors.New("transaction is not fallible")
}

// For UTX validation.
func (a *txAppender) validateNextTx(tx proto.Transaction, currentTimestamp, parentTimestamp uint64, version proto.BlockVersion, acceptFailed bool) error {
	// TODO: Doesn't work correctly if miner doesn't work in NG mode.
	// In this case it returns the last block instead of what is being mined.
	block, err := a.currentBlock()
	if err != nil {
		return errs.Extend(err, "failed get currentBlock")
	}
	blockInfo, err := a.currentBlockInfo()
	if err != nil {
		return errs.Extend(err, "failed get currentBlockInfo")
	}
	rideV5Activated, err := a.stor.features.newestIsActivated(int16(settings.RideV5))
	if err != nil {
		return errs.Extend(err, "failed to check 'RideV5' is activated")
	}
	rideV6Activated, err := a.stor.features.newestIsActivated(int16(settings.RideV6))
	if err != nil {
		return errs.Extend(err, "failed to check 'RideV6' is activated")
	}
	blockRewardDistribution, err := a.stor.features.newestIsActivated(int16(settings.BlockRewardDistribution))
	if err != nil {
		return errs.Extend(err, "failed to check 'BlockRewardDistribution' is activated")
	}
	blockInfo.Timestamp = currentTimestamp
	checkerInfo := &checkerInfo{
		currentTimestamp:        currentTimestamp,
		parentTimestamp:         parentTimestamp,
		blockID:                 block.BlockID(),
		blockVersion:            version,
		height:                  blockInfo.Height,
		rideV5Activated:         rideV5Activated,
		rideV6Activated:         rideV6Activated,
		blockRewardDistribution: blockRewardDistribution,
	}
	blockV5Activated, err := a.stor.features.newestIsActivated(int16(settings.BlockV5))
	if err != nil {
		return errs.Extend(err, "failed to check 'BlockV5' is activated")
	}
	consensusImprovementsActivated, err := a.stor.features.newestIsActivated(int16(settings.ConsensusImprovements))
	if err != nil {
		return errs.Extend(err, "failed to check 'ConsensusImprovements' is activated")
	}
	blockRewardDistributionActivated, err := a.stor.features.newestIsActivated(int16(settings.BlockRewardDistribution))
	if err != nil {
		return errs.Extend(err, "failed to check 'BlockRewardDistribution' is activated")
	}
	invokeExpressionActivated, err := a.stor.features.newestIsActivated(int16(settings.InvokeExpression))
	if err != nil {
		return errs.Extend(err, "failed to check 'InvokeExpression' is activated") // TODO: check feature naming in err message
	}
	issueCounterInBlock := new(proto.StateActionsCounter)
	snapshotApplierInfo := newBlockSnapshotsApplierInfo(checkerInfo, a.settings.AddressSchemeCharacter,
		issueCounterInBlock)
	a.txHandler.tp.snapshotApplier.SetApplierInfo(snapshotApplierInfo)

	appendTxArgs := &appendTxParams{
		chans:                            nil, // nil because validatingUtx == true
		checkerInfo:                      checkerInfo,
		blockInfo:                        blockInfo,
		block:                            block,
		acceptFailed:                     acceptFailed,
		blockV5Activated:                 blockV5Activated,
		rideV5Activated:                  rideV5Activated,
		rideV6Activated:                  rideV6Activated,
		consensusImprovementsActivated:   consensusImprovementsActivated,
		blockRewardDistributionActivated: blockRewardDistributionActivated,
		invokeExpressionActivated:        invokeExpressionActivated,
		validatingUtx:                    true,
		// it's correct to use new counter because there's no block exists, but this field is necessary in tx performer
		stateActionsCounterInBlock: issueCounterInBlock,
	}
	err = a.appendTx(tx, appendTxArgs)
	if err != nil {
		return proto.NewInfoMsg(err)
	}
	return nil
}

func (a *txAppender) reset() {
	a.sc.resetComplexity()
	a.totalScriptsRuns = 0
	a.recentTxIds = make(map[string]struct{})
	a.diffStor.reset()
	a.blockDiffer.reset()
}
