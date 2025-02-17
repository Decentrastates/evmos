package keeper_test

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	abci "github.com/tendermint/tendermint/abci/types"

	"github.com/evmos/evmos/v11/app"
	"github.com/evmos/evmos/v11/contracts"
	"github.com/evmos/evmos/v11/crypto/ethsecp256k1"
	"github.com/evmos/evmos/v11/encoding"
	"github.com/evmos/evmos/v11/server/config"
	"github.com/evmos/evmos/v11/tests"
	"github.com/evmos/evmos/v11/testutil"
	"github.com/evmos/evmos/v11/utils"
	"github.com/evmos/evmos/v11/x/claims/types"
	evm "github.com/evmos/evmos/v11/x/evm/types"
	incentivestypes "github.com/evmos/evmos/v11/x/incentives/types"
	inflationtypes "github.com/evmos/evmos/v11/x/inflation/types"
)

var defaultTxFee = sdk.NewCoin(utils.BaseDenom, sdk.NewInt(1_000_000_000_000_000)) // 0.001 EVMOS

var _ = Describe("Claiming", Ordered, func() {
	claimsAddr := s.app.AccountKeeper.GetModuleAddress(types.ModuleName)
	distrAddr := s.app.AccountKeeper.GetModuleAddress(distrtypes.ModuleName)
	stakeDenom := stakingtypes.DefaultParams().BondDenom
	accountCount := 4

	actionValue := sdk.NewInt(int64(math.Pow10(5) * 10))
	claimValue := actionValue.MulRaw(4)
	totalClaimsAmount := sdk.NewCoin(utils.BaseDenom, claimValue.MulRaw(int64(accountCount)))

	// account initial balances
	initClaimsAmount := sdk.NewInt(types.GenesisDust)
	initBalanceAmount := sdk.NewInt(int64(math.Pow10(18) * 2))
	initStakeAmount := sdk.NewInt(int64(math.Pow10(10) * 2))
	delegateAmount := sdk.NewCoin(utils.BaseDenom, sdk.NewInt(1))
	initBalance := sdk.NewCoins(
		sdk.NewCoin(utils.BaseDenom, initClaimsAmount.Add(initBalanceAmount)), // claimsDenom == evmDenom
	)

	// account for creating the governance proposals
	initClaimsAmount0 := sdk.NewInt(int64(math.Pow10(18) * 2))
	initBalance0 := sdk.NewCoins(
		sdk.NewCoin(stakeDenom, initStakeAmount),
		sdk.NewCoin(utils.BaseDenom, initBalanceAmount.Add(initClaimsAmount0)), // claimsDenom == evmDenom
	)

	var (
		priv0              *ethsecp256k1.PrivKey
		privs              []*ethsecp256k1.PrivKey
		addr0              sdk.AccAddress
		claimsRecords      []types.ClaimsRecord
		params             types.Params
		proposalID         uint64
		totalClaimed       sdk.Coin
		remainderUnclaimed sdk.Coin
		fees               []sdk.Coin
	)

	BeforeAll(func() {
		s.SetupTest()

		params = s.app.ClaimsKeeper.GetParams(s.ctx)
		params.EnableClaims = true
		params.AirdropStartTime = s.ctx.BlockTime()
		err := s.app.ClaimsKeeper.SetParams(s.ctx, params)
		s.Require().NoError(err)

		// mint coins for claiming and send them to the claims module
		coins := sdk.NewCoins(totalClaimsAmount)

		err = testutil.FundModuleAccount(s.ctx, s.app.BankKeeper, inflationtypes.ModuleName, coins)
		s.Require().NoError(err)
		err = s.app.BankKeeper.SendCoinsFromModuleToModule(s.ctx, inflationtypes.ModuleName, types.ModuleName, coins)
		s.Require().NoError(err)

		// fund testing accounts and create claim records
		priv0, _ = ethsecp256k1.GenerateKey()
		addr0 = getAddr(priv0)
		err = testutil.FundAccount(s.ctx, s.app.BankKeeper, addr0, initBalance0)
		s.Require().NoError(err)

		for i := 0; i < accountCount; i++ {
			priv, _ := ethsecp256k1.GenerateKey()
			privs = append(privs, priv)
			addr := getAddr(priv)
			err = testutil.FundAccount(s.ctx, s.app.BankKeeper, addr, initBalance)
			s.Require().NoError(err)
			claimsRecord := types.NewClaimsRecord(claimValue)
			s.app.ClaimsKeeper.SetClaimsRecord(s.ctx, addr, claimsRecord)
			acc := s.app.AccountKeeper.NewAccountWithAddress(s.ctx, addr)
			s.app.AccountKeeper.SetAccount(s.ctx, acc)
			claimsRecords = append(claimsRecords, claimsRecord)

			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom) // claimsDenom == evmDenom == 'aevmos'
			Expect(balance.Amount).To(Equal(initClaimsAmount.Add(initBalanceAmount)))
		}

		// Keep track of the fees paid
		fees = make([]sdk.Coin, len(privs))

		// ensure community pool doesn't have the fund
		poolBalance := s.app.BankKeeper.GetBalance(s.ctx, distrAddr, utils.BaseDenom)
		Expect(poolBalance.IsZero()).To(BeTrue())

		// ensure module account has the escrow fund
		balanceClaims := s.app.BankKeeper.GetBalance(s.ctx, claimsAddr, utils.BaseDenom)
		Expect(balanceClaims).To(Equal(totalClaimsAmount))

		s.Commit()

		proposalID = govProposal(priv0)
	})

	Context("before decay duration", func() {
		var actionV sdk.Coin
		var initialPoolBalance sdk.Coin

		BeforeAll(func() {
			// Community pool will have balance after several blocks because it
			// receives inflation and fees rewards
			initialPoolBalance = s.app.BankKeeper.GetBalance(s.ctx, distrAddr, utils.BaseDenom)
			actionV = sdk.NewCoin(utils.BaseDenom, actionValue)
		})

		It("can claim ActionDelegate", func() {
			addr := getAddr(privs[0])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			delegate(privs[0], delegateAmount)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Add(actionV).Sub(delegateAmount).Sub(defaultTxFee)))
			Expect(balance.Amount).To(Equal(initClaimsAmount.Add(initBalanceAmount).Add(actionV.Amount).Sub(delegateAmount.Amount).Sub(defaultTxFee.Amount)))
			fees[0] = defaultTxFee
		})

		It("can claim ActionEVM", func() {
			addr := getAddr(privs[0])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			fee := getEthTxFee()
			sendEthToSelf(privs[0])
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Add(actionV).Sub(fee)))
			fees[0] = fees[0].Add(fee)
		})

		It("can claim ActionVote", func() {
			addr := getAddr(privs[1])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			vote(privs[1], proposalID)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Add(actionV).Sub(defaultTxFee)))
			fees[1] = defaultTxFee
		})

		It("did not clawback to the community pool", func() {
			// ensure community pool doesn't have the fund
			poolBalance := s.app.BankKeeper.GetBalance(s.ctx, distrAddr, utils.BaseDenom)
			Expect((poolBalance.Sub(initialPoolBalance)).IsZero()).To(BeTrue())

			// ensure module account has the escrow fund minus what was claimed
			balanceClaims := s.app.BankKeeper.GetBalance(s.ctx, claimsAddr, utils.BaseDenom)
			totalClaimed = sdk.NewCoin(utils.BaseDenom, actionV.Amount.MulRaw(3))
			Expect(balanceClaims).To(Equal(totalClaimsAmount.Sub(totalClaimed)))
		})
	})

	Context("at 2/3 decay duration", func() {
		var actionV sdk.Coin
		var unclaimedV sdk.Coin
		var initialPoolBalance sdk.Coin

		BeforeAll(func() {
			actionV = sdk.NewCoin(utils.BaseDenom, actionValue.QuoRaw(3))
			unclaimedV = sdk.NewCoin(utils.BaseDenom, actionValue.Sub(actionV.Amount))
			duration := params.DecayStartTime().Sub(s.ctx.BlockHeader().Time)

			s.CommitAfter(duration)
			duration = params.GetDurationOfDecay() * 2 / 3

			// create another proposal to vote for
			testTime := s.ctx.BlockHeader().Time.Add(duration)
			s.CommitAfter(duration - time.Hour)
			proposalID = govProposal(priv0)
			s.CommitAfter(testTime.Sub(s.ctx.BlockHeader().Time))

			// Community pool will have balance after several blocks because it
			// receives inflation and fees rewards
			initialPoolBalance = s.app.BankKeeper.GetBalance(s.ctx, distrAddr, utils.BaseDenom)
		})

		It("can claim ActionDelegate", func() {
			addr := getAddr(privs[1])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			delegate(privs[1], delegateAmount)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Add(actionV).Sub(delegateAmount).Sub(defaultTxFee)))
			fees[1] = fees[1].Add(defaultTxFee)
		})

		It("can claim ActionEVM", func() {
			addr := getAddr(privs[1])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			fee := getEthTxFee()
			sendEthToSelf(privs[1])
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Add(actionV).Sub(fee)))
			fees[1] = fees[1].Add(fee)
			fee = getEthTxFee()
			sendEthToSelf(privs[2])
			fees[2] = fee
		})

		It("can claim ActionVote", func() {
			addr := getAddr(privs[0])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			vote(privs[0], proposalID)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Add(actionV).Sub(defaultTxFee)))
			fees[0] = fees[0].Add(defaultTxFee)
		})

		It("cannot claim ActionDelegate a second time", func() {
			addr := getAddr(privs[1])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			delegate(privs[1], delegateAmount)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Sub(delegateAmount).Sub(defaultTxFee)))
			fees[1] = fees[1].Add(defaultTxFee)
		})

		It("cannot claim ActionEVM a second time", func() {
			addr := getAddr(privs[1])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			fee := getEthTxFee()
			sendEthToSelf(privs[1])
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Sub(fee)))
			fees[1] = fees[1].Add(fee)
		})

		It("cannot claim ActionVote a second time", func() {
			addr := getAddr(privs[0])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			vote(privs[0], proposalID)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Sub(defaultTxFee)))
			fees[0] = fees[0].Add(defaultTxFee)
		})

		It("did not clawback to the community pool", func() {
			remainderUnclaimed = sdk.NewCoin(utils.BaseDenom, unclaimedV.Amount.MulRaw(4))
			totalClaimed = totalClaimed.Add(sdk.NewCoin(utils.BaseDenom, actionV.Amount.MulRaw(4)))

			// ensure community pool doesn't have the fund
			poolBalance := s.app.BankKeeper.GetBalance(s.ctx, distrAddr, utils.BaseDenom)
			Expect(poolBalance.Sub(initialPoolBalance)).To(Equal(remainderUnclaimed))

			// ensure module account has the escrow fund minus what was claimed
			balanceClaims := s.app.BankKeeper.GetBalance(s.ctx, claimsAddr, utils.BaseDenom)
			Expect(balanceClaims).To(Equal(totalClaimsAmount.Sub(totalClaimed).Sub(remainderUnclaimed)))
		})
	})

	Context("after decay duration", func() {
		BeforeAll(func() {
			duration := params.AirdropEndTime().Sub(s.ctx.BlockHeader().Time) + 1
			s.CommitAfter(duration)

			// ensure module account has the unclaimed amount before airdrop ends
			moduleBalances := s.app.ClaimsKeeper.GetModuleAccountBalances(s.ctx)
			Expect(moduleBalances.AmountOf(utils.BaseDenom)).To(Equal(totalClaimsAmount.Sub(totalClaimed).Sub(remainderUnclaimed).Amount))

			s.Commit()
		})

		It("cannot claim additional actions", func() {
			addr := getAddr(privs[2])
			prebalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			delegate(privs[2], delegateAmount)
			balance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(balance).To(Equal(prebalance.Sub(delegateAmount).Sub(defaultTxFee)))
			fees[2] = fees[2].Add(defaultTxFee)
		})

		It("cannot clawback already claimed actions", func() {
			addr := getAddr(privs[0])
			finalBalance := s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			claimed := actionValue.MulRaw(2).Add(actionValue.QuoRaw(3))
			Expect(finalBalance.Amount).To(Equal(initClaimsAmount.Add(initBalanceAmount).Add(claimed).Sub(delegateAmount.Amount).Sub(fees[0].Amount)))

			addr = getAddr(privs[1])
			finalBalance = s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			claimed = actionValue.MulRaw(2).QuoRaw(3).Add(actionValue)
			Expect(finalBalance.Amount).To(Equal(initClaimsAmount.Add(initBalanceAmount).Add(claimed).Sub(delegateAmount.Amount.MulRaw(2)).Sub(fees[1].Amount)))

			addr = getAddr(privs[2])
			finalBalance = s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			claimed = actionValue.QuoRaw(3)
			Expect(finalBalance.Amount).To(Equal(initClaimsAmount.Add(initBalanceAmount).Add(claimed).Sub(delegateAmount.Amount).Sub(fees[2].Amount)))

			// no-op, should have same balance as initial balance
			addr = getAddr(privs[3])
			finalBalance = s.app.BankKeeper.GetBalance(s.ctx, addr, utils.BaseDenom)
			Expect(finalBalance.Amount).To(Equal(initClaimsAmount.Add(initBalanceAmount)))
		})

		It("can clawback unclaimed", func() {
			// ensure module account is empty
			moduleBalance := s.app.ClaimsKeeper.GetModuleAccountBalances(s.ctx)
			Expect(moduleBalance.AmountOf(utils.BaseDenom).IsZero()).To(BeTrue())
		})
	})
})

func getAddr(priv *ethsecp256k1.PrivKey) sdk.AccAddress {
	return sdk.AccAddress(priv.PubKey().Address().Bytes())
}

func delegate(priv *ethsecp256k1.PrivKey, delegateAmount sdk.Coin) {
	accountAddress := sdk.AccAddress(priv.PubKey().Address().Bytes())

	val, err := sdk.ValAddressFromBech32(s.validator.OperatorAddress)
	s.Require().NoError(err)

	delegateMsg := stakingtypes.NewMsgDelegate(accountAddress, val, delegateAmount)
	deliverTx(priv, delegateMsg)
}

func govProposal(priv *ethsecp256k1.PrivKey) uint64 {
	stakeDenom := stakingtypes.DefaultParams().BondDenom
	accountAddress := sdk.AccAddress(priv.PubKey().Address().Bytes())
	contractAddress := deployContract(priv)
	content := incentivestypes.NewRegisterIncentiveProposal(
		"test",
		"description",
		contractAddress.String(),
		sdk.DecCoins{sdk.NewDecCoinFromDec(utils.BaseDenom, sdk.NewDecWithPrec(5, 2))},
		1000,
	)

	deposit := sdk.NewCoins(sdk.NewCoin(stakeDenom, sdk.NewInt(100000000)))
	msg, err := govv1beta1.NewMsgSubmitProposal(content, deposit, accountAddress)
	s.Require().NoError(err)

	res := deliverTx(priv, msg)
	submitEvent := res.GetEvents()[8]
	Expect(submitEvent.Type).To(Equal("submit_proposal"))
	Expect(string(submitEvent.Attributes[0].Key)).To(Equal("proposal_id"))

	proposalID, err := strconv.ParseUint(string(submitEvent.Attributes[0].Value), 10, 64)
	s.Require().NoError(err)

	return proposalID
}

func vote(priv *ethsecp256k1.PrivKey, proposalID uint64) {
	accountAddress := sdk.AccAddress(priv.PubKey().Address().Bytes())

	voteMsg := govv1beta1.NewMsgVote(accountAddress, proposalID, govv1beta1.OptionAbstain)
	deliverTx(priv, voteMsg)
}

func sendEthToSelf(priv *ethsecp256k1.PrivKey) {
	chainID := s.app.EvmKeeper.ChainID()
	from := common.BytesToAddress(priv.PubKey().Address().Bytes())
	nonce := s.app.EvmKeeper.GetNonce(s.ctx, from)

	msgEthereumTx := evm.NewTx(chainID, nonce, &from, nil, 100000, nil, s.app.FeeMarketKeeper.GetBaseFee(s.ctx), big.NewInt(1), nil, &ethtypes.AccessList{})
	msgEthereumTx.From = from.String()
	performEthTx(priv, msgEthereumTx)
}

func deployContract(priv *ethsecp256k1.PrivKey) common.Address {
	chainID := s.app.EvmKeeper.ChainID()
	from := common.BytesToAddress(priv.PubKey().Address().Bytes())
	nonce := s.app.EvmKeeper.GetNonce(s.ctx, from)

	ctorArgs, err := contracts.ERC20MinterBurnerDecimalsContract.ABI.Pack("", "Test", "TTT", uint8(18))
	s.Require().NoError(err)

	data := append(contracts.ERC20MinterBurnerDecimalsContract.Bin, ctorArgs...) //nolint:gocritic
	args, err := json.Marshal(&evm.TransactionArgs{
		From: &s.address,
		Data: (*hexutil.Bytes)(&data),
	})
	s.Require().NoError(err)

	ctx := sdk.WrapSDKContext(s.ctx)
	res, err := s.queryClientEvm.EstimateGas(ctx, &evm.EthCallRequest{
		Args:   args,
		GasCap: config.DefaultGasCap,
	})
	s.Require().NoError(err)

	msgEthereumTx := evm.NewTxContract(chainID, nonce, nil, res.Gas, nil, s.app.FeeMarketKeeper.GetBaseFee(s.ctx), big.NewInt(1), data, &ethtypes.AccessList{})
	msgEthereumTx.From = from.String()

	performEthTx(priv, msgEthereumTx)
	s.Commit()

	contractAddress := crypto.CreateAddress(from, nonce)
	acc := s.app.EvmKeeper.GetAccountWithoutBalance(s.ctx, contractAddress)
	s.Require().NotEmpty(acc)
	s.Require().True(acc.IsContract())
	return contractAddress
}

func performEthTx(priv *ethsecp256k1.PrivKey, msgEthereumTx *evm.MsgEthereumTx) {
	// Sign transaction
	err := msgEthereumTx.Sign(s.ethSigner, tests.NewSigner(priv))
	s.Require().NoError(err)

	// Assemble transaction from fields
	encodingConfig := encoding.MakeConfig(app.ModuleBasics)
	txBuilder := encodingConfig.TxConfig.NewTxBuilder()
	tx, err := msgEthereumTx.BuildTx(txBuilder, utils.BaseDenom)
	s.Require().NoError(err)

	// Encode transaction by default Tx encoder and broadcasted over the network
	txEncoder := encodingConfig.TxConfig.TxEncoder()
	bz, err := txEncoder(tx)
	s.Require().NoError(err)

	req := abci.RequestDeliverTx{Tx: bz}
	res := s.app.BaseApp.DeliverTx(req)
	Expect(res.IsOK()).To(Equal(true))
}

func deliverTx(priv *ethsecp256k1.PrivKey, msgs ...sdk.Msg) abci.ResponseDeliverTx {
	encodingConfig := encoding.MakeConfig(app.ModuleBasics)
	accountAddress := sdk.AccAddress(priv.PubKey().Address().Bytes())

	txBuilder := encodingConfig.TxConfig.NewTxBuilder()

	txBuilder.SetGasLimit(1000000)
	txBuilder.SetFeeAmount(sdk.Coins{defaultTxFee})

	err := txBuilder.SetMsgs(msgs...)
	s.Require().NoError(err)

	seq, err := s.app.AccountKeeper.GetSequence(s.ctx, accountAddress)
	s.Require().NoError(err)

	// First round: we gather all the signer infos. We use the "set empty
	// signature" hack to do that.
	sigV2 := signing.SignatureV2{
		PubKey: priv.PubKey(),
		Data: &signing.SingleSignatureData{
			SignMode:  encodingConfig.TxConfig.SignModeHandler().DefaultMode(),
			Signature: nil,
		},
		Sequence: seq,
	}

	sigsV2 := []signing.SignatureV2{sigV2}

	err = txBuilder.SetSignatures(sigsV2...)
	s.Require().NoError(err)

	// Second round: all signer infos are set, so each signer can sign.
	accNumber := s.app.AccountKeeper.GetAccount(s.ctx, accountAddress).GetAccountNumber()
	signerData := authsigning.SignerData{
		ChainID:       s.ctx.ChainID(),
		AccountNumber: accNumber,
		Sequence:      seq,
	}
	sigV2, err = tx.SignWithPrivKey(
		encodingConfig.TxConfig.SignModeHandler().DefaultMode(), signerData,
		txBuilder, priv, encodingConfig.TxConfig,
		seq,
	)
	s.Require().NoError(err)

	sigsV2 = []signing.SignatureV2{sigV2}
	err = txBuilder.SetSignatures(sigsV2...)
	s.Require().NoError(err)

	// bz are bytes to be broadcasted over the network
	bz, err := encodingConfig.TxConfig.TxEncoder()(txBuilder.GetTx())
	s.Require().NoError(err)

	req := abci.RequestDeliverTx{Tx: bz}
	res := s.app.BaseApp.DeliverTx(req)
	Expect(res.IsOK()).To(Equal(true), res.Log)
	return res
}

func getEthTxFee() sdk.Coin {
	baseFee := s.app.FeeMarketKeeper.GetBaseFee(s.ctx)
	baseFee.Mul(baseFee, big.NewInt(100000))
	feeAmt := baseFee.Quo(baseFee, big.NewInt(2))
	return sdk.NewCoin(utils.BaseDenom, sdkmath.NewIntFromBigInt(feeAmt))
}
