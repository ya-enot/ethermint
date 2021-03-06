package app

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rpc"
	cli "gopkg.in/urfave/cli.v1"

	"github.com/ya-enot/etherus/ethereum"
	"github.com/ya-enot/etherus/ethereum/validators"
	emtTypes "github.com/ya-enot/etherus/types"

	cosmosErrors "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"
	abciTypes "github.com/tendermint/tendermint/abci/types"
	tmLog "github.com/tendermint/tendermint/libs/log"

	ethUtils "github.com/ethereum/go-ethereum/cmd/utils"
	emtUtils "github.com/ya-enot/etherus/cmd/utils"
)

// EthermintApplication implements an ABCI application
// #stable - 0.4.0
type EthermintApplication struct {

	// backend handles the ethereum state machine
	// and wrangles other services started by an ethereum node (eg. tx pool)
	backend *ethereum.Backend // backend ethereum struct

	// a closure to return the latest current state from the ethereum blockchain
	getCurrentState func() (*state.StateDB, error)

	checkTxState *state.StateDB

	// an ethereum rpc client we can forward queries to
	rpcClient *rpc.Client

	// strategy for validator compensation
	strategy *emtTypes.Strategy

	logger tmLog.Logger

	validators        *validators.ValidatorsManager
	validatorsHistory *validators.ValidatorsHistory

	myValidator *common.Address

	requestBeginBlock *abciTypes.RequestBeginBlock

	appDb *ethdb.LDBDatabase
}

// NewEthermintApplication creates a fully initialised instance of EthermintApplication
// #stable - 0.4.0
func NewEthermintApplication(ctx *cli.Context, backend *ethereum.Backend,
	client *rpc.Client, myValidator *common.Address, strategy *emtTypes.Strategy) (*EthermintApplication, error) {

	state, err := backend.Ethereum().BlockChain().State()
	if err != nil {
		return nil, err
	}

	vlds, err := validators.CreateValidators(backend, client)
	if err != nil {
		return nil, err
	}

	ethermintDataDir := emtUtils.MakeDataDir(ctx)

	appDb, err := ethdb.NewLDBDatabase(filepath.Join(ethermintDataDir,
		"etherus/appdata"), 0, 0)

	if err != nil {
		ethUtils.Fatalf("could not open app database: %v", err)
	}

	app := &EthermintApplication{
		backend:         backend,
		rpcClient:       client,
		getCurrentState: backend.Ethereum().BlockChain().State,
		checkTxState:    state.Copy(),
		strategy:        strategy,
		validators:      vlds,
		//		validatorsHistory: validators.NewValidatorsHistory(&appDb),
		myValidator: myValidator,
		appDb:       appDb,
	}

	if err := app.backend.InitEthState(common.Address{}); err != nil {
		return nil, err
	}

	return app, nil
}

// SetLogger sets the logger for the ethermint application
// #unstable
func (app *EthermintApplication) SetLogger(log tmLog.Logger) {
	app.logger = log
}

var bigZero = big.NewInt(0)

// maxTransactionSize is 32KB in order to prevent DOS attacks
const maxTransactionSize = 32768

// Info returns information about the last height and app_hash to the tendermint engine
// #stable - 0.4.0

func (app *EthermintApplication) Info(req abciTypes.RequestInfo) abciTypes.ResponseInfo {
	blockchain := app.backend.Ethereum().BlockChain()
	currentBlock := blockchain.CurrentBlock()
	height := currentBlock.Number()
	hash := currentBlock.Hash()

	app.logger.Debug("Info", "height", height) // nolint: errcheck

	// This check determines whether it is the first time ethermint gets started.
	// If it is the first time, then we have to respond with an empty hash, since
	// that is what tendermint expects.
	if height.Cmp(bigZero) == 0 {
		return abciTypes.ResponseInfo{
			Data:             "ABCIEthereum",
			LastBlockHeight:  height.Int64(),
			LastBlockAppHash: []byte{},
		}
	}

	return abciTypes.ResponseInfo{
		Data:             "ABCIEthereum",
		LastBlockHeight:  height.Int64(),
		LastBlockAppHash: hash[:],
	}
}

// SetOption sets a configuration option
// #stable - 0.4.0
func (app *EthermintApplication) SetOption(req abciTypes.RequestSetOption) abciTypes.ResponseSetOption {

	app.logger.Debug("SetOption", "key", req.GetKey(), "value", req.GetValue()) // nolint: errcheck
	return abciTypes.ResponseSetOption{}
}

// InitChain initializes the validator set
// #stable - 0.4.0
func (app *EthermintApplication) InitChain(req abciTypes.RequestInitChain) abciTypes.ResponseInitChain {

	app.logger.Debug("InitChain") // nolint: errcheck
	app.SetValidators(req.GetValidators())
	return abciTypes.ResponseInitChain{}
}

// CheckTx checks a transaction is valid but does not mutate the state
// #stable - 0.4.0
func (app *EthermintApplication) CheckTx(txBytes []byte) abciTypes.ResponseCheckTx {
	tx, err := decodeTx(txBytes)
	if err != nil {
		// nolint: errcheck
		app.logger.Debug("CheckTx: Received invalid transaction", "tx", tx)
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeInternal),
			Log:  err.Error(),
		}
	}
	app.logger.Debug("CheckTx: Received valid transaction", "tx", tx) // nolint: errcheck

	return app.validateTx(tx)
}

// DeliverTx executes a transaction against the latest state
// #stable - 0.4.0
func (app *EthermintApplication) DeliverTx(txBytes []byte) abciTypes.ResponseDeliverTx {
	tx, err := decodeTx(txBytes)
	if err != nil {
		// nolint: errcheck
		app.logger.Debug("DelivexTx: Received invalid transaction", "tx", tx, "err", err)
		return abciTypes.ResponseDeliverTx{
			Code: uint32(cosmosErrors.CodeInternal),
			Log:  err.Error(),
		}
	}
	app.logger.Debug("DeliverTx: Received valid transaction", "tx", tx) // nolint: errcheck

	res := app.backend.DeliverTx(tx)
	if res.IsErr() {
		// nolint: errcheck
		app.logger.Error("DeliverTx: Error delivering tx to ethereum backend", "tx", tx,
			"err", err)
		return res
	}
	app.CollectTx(tx)

	return abciTypes.ResponseDeliverTx{
		Code: abciTypes.CodeTypeOK,
	}
}

// BeginBlock starts a new Ethereum block
// #stable - 0.4.0
func (app *EthermintApplication) BeginBlock(beginBlock abciTypes.RequestBeginBlock) abciTypes.ResponseBeginBlock {

	app.logger.Debug("BeginBlock") // nolint: errcheck

	app.requestBeginBlock = &beginBlock

	validatorAddress := common.BytesToAddress(beginBlock.Header.Proposer.Address)
	app.logger.Debug("Proposer address is ", "validatorAddress", validatorAddress)

	if err := app.backend.InitEthState(app.Receiver()); err != nil {
		panic(err)
	}

	// update the eth header with the tendermint header
	app.backend.UpdateHeaderWithTimeInfo(beginBlock.GetHeader())
	return abciTypes.ResponseBeginBlock{}
}

// EndBlock accumulates rewards for the validators and updates them
// #stable - 0.4.0
func (app *EthermintApplication) EndBlock(endBlock abciTypes.RequestEndBlock) abciTypes.ResponseEndBlock {
	app.logger.Debug("EndBlock", "height", endBlock.GetHeight()) // nolint: errcheck
	app.backend.AccumulateRewards(app.strategy)
	return app.GetUpdatedValidators()
}

// Commit commits the block and returns a hash of the current state
// #stable - 0.4.0
func (app *EthermintApplication) Commit() abciTypes.ResponseCommit {
	app.logger.Debug("Commit") // nolint: errcheck
	blockHash, err := app.backend.Commit(app.Receiver())
	rootHash := app.backend.Ethereum().BlockChain().CurrentBlock().Root()
	app.logger.Info("Commiting ", "blockHash", hex.EncodeToString(blockHash[:]), "cbr", hex.EncodeToString(rootHash[:])) // nolint: errcheck
	if err != nil {
		// nolint: errcheck
		app.logger.Error("Error getting latest ethereum state", "err", err)
		panic(errors.New("Error getting latest ethereum state"))
	}

	state, err := app.getCurrentState()
	if err != nil {
		app.logger.Error("Error getting latest state", "err", err) // nolint: errcheck
		panic(errors.New("Error getting latest state"))
	}

	app.checkTxState = state.Copy()
	app.requestBeginBlock = nil

	return abciTypes.ResponseCommit{
		Data: blockHash[:],
	}
}

// Query queries the state of the EthermintApplication
// #stable - 0.4.0
func (app *EthermintApplication) Query(query abciTypes.RequestQuery) abciTypes.ResponseQuery {
	app.logger.Debug("Query") // nolint: errcheck
	var in jsonRequest
	if err := json.Unmarshal(query.Data, &in); err != nil {
		return abciTypes.ResponseQuery{Code: uint32(cosmosErrors.CodeInternal),
			Log: err.Error()}
	}
	var result interface{}
	if err := app.rpcClient.Call(&result, in.Method, in.Params...); err != nil {
		return abciTypes.ResponseQuery{Code: uint32(cosmosErrors.CodeInternal),
			Log: err.Error()}
	}
	bytes, err := json.Marshal(result)
	if err != nil {
		return abciTypes.ResponseQuery{Code: uint32(cosmosErrors.CodeInternal),
			Log: err.Error()}
	}
	return abciTypes.ResponseQuery{Code: abciTypes.CodeTypeOK, Value: bytes}
}

//-------------------------------------------------------

// validateTx checks the validity of a tx against the blockchain's current state.
// it duplicates the logic in ethereum's tx_pool
func (app *EthermintApplication) validateTx(tx *ethTypes.Transaction) abciTypes.ResponseCheckTx {

	// Heuristic limit, reject transactions over 32KB to prevent DOS attacks
	if tx.Size() > maxTransactionSize {
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeInternal),
			Log:  core.ErrOversizedData.Error()}
	}

	var signer ethTypes.Signer = ethTypes.FrontierSigner{}
	if tx.Protected() {
		signer = ethTypes.NewEIP155Signer(tx.ChainId())
	}

	// Make sure the transaction is signed properly
	from, err := ethTypes.Sender(signer, tx)
	if err != nil {
		// TODO: Add cosmosErrors.CodeTypeInvalidSignature ?
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeInternal),
			Log:  core.ErrInvalidSender.Error()}
	}

	// Transactions can't be negative. This may never happen using RLP decoded
	// transactions but may occur if you create a transaction using the RPC.
	if tx.Value().Sign() < 0 {
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeUnknownRequest),
			Log:  core.ErrNegativeValue.Error()}
	}

	currentState := app.checkTxState

	// Make sure the account exist - cant send from non-existing account.
	if !currentState.Exist(from) {
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeUnknownAddress),
			Log:  core.ErrInvalidSender.Error()}
	}

	// Check the transaction doesn't exceed the current block limit gas.
	gasLimit := app.backend.GasLimit()
	if gasLimit < tx.Gas() {
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeOutOfGas),
			Log:  core.ErrGasLimitReached.Error()}
	}

	// Check if nonce is not strictly increasing
	nonce := currentState.GetNonce(from)
	if nonce != tx.Nonce() {
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeInternal),
			Log: fmt.Sprintf(
				"Nonce not strictly increasing. Expected %d Got %d",
				nonce, tx.Nonce())}
	}

	// Transactor should have enough funds to cover the costs
	// cost == V + GP * GL
	currentBalance := currentState.GetBalance(from)
	if currentBalance.Cmp(tx.Cost()) < 0 {
		return abciTypes.ResponseCheckTx{
			// TODO: Add cosmosErrors.CodeTypeInsufficientFunds ?
			Code: uint32(cosmosErrors.CodeUnknownRequest),
			Log: fmt.Sprintf(
				"Current balance: %s, tx cost: %s",
				currentBalance, tx.Cost())}
	}

	intrGas, err := core.IntrinsicGas(tx.Data(), tx.To() == nil, true) // homestead == true
	if err != nil {
		app.logger.Error("Intrinsic gas failed: ", "err", err)
		panic("Intrinsic gas failed!")
	}
	if tx.Gas() < intrGas {
		return abciTypes.ResponseCheckTx{
			Code: uint32(cosmosErrors.CodeUnknownRequest),
			Log:  core.ErrIntrinsicGas.Error()}
	}

	// Update ether balances
	// amount + gasprice * gaslimit
	currentState.SubBalance(from, tx.Cost())
	// tx.To() returns a pointer to a common address. It returns nil
	// if it is a contract creation transaction.
	if to := tx.To(); to != nil {
		currentState.AddBalance(*to, tx.Value())
	}
	currentState.SetNonce(from, tx.Nonce()+1)

	return abciTypes.ResponseCheckTx{Code: abciTypes.CodeTypeOK}
}
