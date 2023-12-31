package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/algorand/go-algorand-sdk/client/algod/models"
	"github.com/algorand/go-algorand-sdk/v2/abi"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/mnemonic"
	"github.com/algorand/go-algorand-sdk/v2/transaction"
	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/joho/godotenv"
)

var minerMnemonic string
var minerAccount crypto.Account
var depositAddress types.Address
var contract abi.Contract
var method abi.Method

var tpm uint64
var fee uint64
var network string
var appID uint64

func init() {
	flag.Uint64Var(&tpm, "tpm", 1, "Transactions per minute.")
	flag.Uint64Var(&fee, "fee", 2000, "Fee per transaction (micro algos).")
	flag.StringVar(&network, "network", "", "Algorand network.")
	flag.Parse()

	godotenv.Load()

	if network != "mainnet" && network != "testnet" {
		slog.Error("network flag must be set to 'mainnet' or 'testnet'", "network", network)
		panic("stopping execution because network flag is invalid")
	}

	if tpm < 1 {
		slog.Error("invalid tpm value", "tpm", tpm)
		panic("stopping execution because tpm value is invalid")
	}

	if fee < 1000 || fee > 20000 {
		slog.Error("invalid fee value. fee must be at least 2000 micro algos and no greater than 20000 micro algos", "fee", fee)
		panic("stopping execution because fee value is invalid")
	}

	minerMnemonic = os.Getenv("MINER_MNEMONIC")
	if minerMnemonic == "" {
		slog.Error("MINER_MNEMONIC environment variable must be set")
		panic("stopping execution because MINER_MNEMONIC was not found")
	}

	var err error
	minerSk, err := mnemonic.ToPrivateKey(minerMnemonic)
	if err != nil {
		slog.Error("failed to convert MINER_MNEMONIC to private key", "err", err)
		panic("stopping execution because MINER_MNEMONIC failed to convert to private key")
	}

	minerAccount, err = crypto.AccountFromPrivateKey(minerSk)
	if err != nil {
		slog.Error("failed to get account from miner secret key", "err", err)
		panic("stopping execution because miner account retreival failed")
	}

	da := os.Getenv("DEPOSIT_ADDRESS")
	if da == "" {
		slog.Error("DEPOSIT_ADDRESS environment variable must be set")
		panic("stopping execution because DEPOSIT_ADDRESS was not found")
	}

	depositAddress, err = types.DecodeAddress(da)
	if err != nil {
		slog.Error("failed to decode address")
		panic("stopping execution because deposit address decoding failed")
	}

	b, err := os.ReadFile("abi.json")
	if err != nil {
		slog.Error("failed to read abi.json file", "err", err)
		panic("stopping execution because read failed on abi.json")
	}

	err = json.Unmarshal(b, &contract)
	if err != nil {
		slog.Error("failed to unmarshal abi.json to abi contract", "err", err)
		panic("stopping execution because unmarshalling to abi contract failed")
	}

	appIdName := "APP_TESTNET"
	if network == "mainnet" {
		appIdName = "APP_MAINNET"
	}

	id := os.Getenv(appIdName)
	if id == "" {
		slog.Error("app id environment variable must be set", "envVarName", appIdName)
		panic("stopping execution because app id was not found")
	}

	appID, err = strconv.ParseUint(id, 10, 64)
	if err != nil {
		slog.Error("failed to parse app id to uint64", "err", err, "envVarName", appIdName, "appID", id)
		panic("stopping execution because parsing id to uint64 failed")
	}

	method, err = contract.GetMethodByName("mine")
	if err != nil {
		slog.Error("failed to get method from contract", "methodName", "mine")
		panic("stopping execution because method retrieval failed")
	}

	slog.Info("Parameters found", "minerAddress", minerAccount.Address.String(), "depositAddress", depositAddress, "tpm", tpm, "fee", fee, "network", network, "appID", appID)
}

func main() {
	ctx := context.Background()
	client := makeClient()
	checkNodeConnection(ctx, client)

	miner := Miner{
		client:         client,
		tpm:            tpm,
		fee:            fee,
		appID:          appID,
		method:         method,
		minerAccount:   minerAccount,
		depositAddress: depositAddress,
		total:          &atomic.Uint64{},
	}

	miner.checkDepositOptedIn(ctx)
	miner.checkMiner(ctx)
	go miner.mine(ctx)

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

func makeClient() *algod.Client {
	if network == "mainnet" {
		token := os.Getenv("ALGOD_MAINNET_TOKEN")
		if token == "" {
			slog.Warn("ALGOD_MAINNET_TOKEN is not set. Continuing as this may be intended...")
		}

		server := os.Getenv("ALGOD_MAINNET_SERVER")
		if server == "" {
			slog.Error("ALGOD_MAINNET_SERVER environment variable must be set")
			panic("stopping execution because ALGOD_MAINNET_SERVER was not found")
		}

		port := os.Getenv("ALGOD_MAINNET_PORT")
		if port == "" {
			slog.Error("ALGOD_MAINNET_PORT environment variable must be set")
			panic("stopping execution because ALGOD_MAINNET_PORT was not found")
		}

		customTransport := http.DefaultTransport.(*http.Transport).Clone()
		customTransport.MaxIdleConns = 100
		customTransport.MaxConnsPerHost = 100
		customTransport.MaxIdleConnsPerHost = 100
		client, err := algod.MakeClientWithTransport(fmt.Sprintf("%s:%s", server, port), token, nil, customTransport)
		if err != nil {
			slog.Error("failed to make algod mainnet client", "err", err)
			panic("stopping execution because algod client failed creation")
		}

		return client
	}

	token := os.Getenv("ALGOD_TESTNET_TOKEN")
	if token == "" {
		slog.Warn("ALGOD_TESTNET_TOKEN is not set. Continuing as this may be intended...")
	}

	server := os.Getenv("ALGOD_TESTNET_SERVER")
	if server == "" {
		slog.Error("ALGOD_TESTNET_SERVER environment variable must be set")
		panic("stopping execution because ALGOD_TESTNET_SERVER was not found")
	}

	port := os.Getenv("ALGOD_TESTNET_PORT")
	if port == "" {
		slog.Error("ALGOD_TESTNET_PORT environment variable must be set")
		panic("stopping execution because ALGOD_TESTNET_PORT was not found")
	}

	customTransport := http.DefaultTransport.(*http.Transport).Clone()
	customTransport.MaxIdleConns = 100
	customTransport.MaxConnsPerHost = 100
	customTransport.MaxIdleConnsPerHost = 100
	client, err := algod.MakeClientWithTransport(fmt.Sprintf("%s:%s", server, port), token, nil, customTransport)
	if err != nil {
		slog.Error("failed to make algod testnet client", "err", err)
		panic("stopping execution because algod client failed creation")
	}

	return client
}

func checkNodeConnection(ctx context.Context, client *algod.Client) {
	status, err := client.Status().Do(ctx)
	if err != nil {
		slog.Error("failed to get node status. please update node connectivity settings", "err", err)
		panic("stopping execution because algod client failed creation")
	}

	slog.Info("Node connected successfully", "Block", status.LastRound)
}

type Miner struct {
	client *algod.Client

	tpm            uint64
	fee            uint64
	appID          uint64
	method         abi.Method
	minerAccount   crypto.Account
	depositAddress types.Address

	total *atomic.Uint64
}

type AccountWithMinBalance struct {
	models.Account
	MinBalance uint64 `json:"min-balance,omitempty"`
}

func (m *Miner) getBareAccount(ctx context.Context, account types.Address) (AccountWithMinBalance, error) {
	var response AccountWithMinBalance
	var params = algod.AccountInformationParams{
		Exclude: "all",
	}

	err := (*common.Client)(m.client).Get(ctx, &response, fmt.Sprintf("/v2/accounts/%s", account.String()), params, nil)
	if err != nil {
		return AccountWithMinBalance{}, err
	}
	return response, nil
}

func (m *Miner) checkMiner(ctx context.Context) {
	minerInfo, err := m.getBareAccount(ctx, m.minerAccount.Address)
	if err != nil {
		slog.Error("failed to get miner account info", "err", err)
		panic("stopping execution because getting miner account info failed")
	}

	minerBalance := minerInfo.Amount - minerInfo.MinBalance
	if minerBalance < 1000000 {
		slog.Error("miner has low balance, please fund before mining", "balance", float64(minerBalance)/math.Pow10(6))
		panic("stopping execution because miner has low balance")
	}

	cost := uint64(tpm) * fee
	slog.Info(fmt.Sprintf("Miner will send %d transactions per minutes with %f fee (%f ALGO cost per minute)", tpm, float64(fee)/math.Pow10(6), float64(cost)/math.Pow10(6)))
	slog.Info(fmt.Sprintf("Miner will spend %f ALGO", float64(minerBalance)/math.Pow10(6)))
	if fee == 1000 {
		slog.Info("Since you set fee to minimum (1000), you will be spending slightly more and have a slightly shorter run time than calculated")
	}

	minerSeconds := minerBalance / (cost / 60)
	minerMinutes := (minerSeconds % 3600) / 60
	minerHours := (minerSeconds % 86400) / 3600
	minerDays := minerSeconds / 86400

	slog.Info("Miner run time", "days", minerDays, "hours", minerHours, "minutes", minerMinutes)
}

func (m *Miner) getMinerBalance(ctx context.Context) (uint64, error) {
	minerInfo, err := m.getBareAccount(ctx, m.minerAccount.Address)
	if err != nil {
		return 0, err
	}

	minerBalance := minerInfo.Amount - minerInfo.MinBalance
	return minerBalance, nil
}

func (m *Miner) checkDepositOptedIn(ctx context.Context) {
	depositInfo, err := m.client.AccountInformation(depositAddress.String()).Do(ctx)
	if err != nil {
		slog.Error("failed to get deposit address info", "err", err)
		panic("stopping execution because getting deposit account info failed")
	}
	appInfo, err := m.getApplicationData(ctx)
	if err != nil {
		slog.Error("failed to get application data", "err", err)
		panic("stopping execution because retreiving application data failed")
	}

	found := false

	for _, a := range depositInfo.AppsLocalState {
		if a.Id == appInfo.ID {
			found = true
			break
		}
	}

	if !found {
		slog.Error("deposit address not opted in to app", "appId", appInfo.ID)
		panic("stopping execution because deposit address is not opted into app")
	}

	found = false

	for _, a := range depositInfo.Assets {
		if a.AssetId == appInfo.Asset {
			found = true
			break
		}
	}

	if !found {
		slog.Error("deposit address not opted in to asset", "assetId", appInfo.Asset)
		panic("stopping execution because deposit address is not opted into asset")
	}
}

type AppData struct {
	ID                 uint64
	Asset              uint64
	Block              uint64
	TotalEffort        uint64
	TotalTransactions  uint64
	Halving            uint64
	HalvingSupply      uint64
	MinedSupply        uint64
	MinerReward        uint64
	LastMiner          string
	LastMinerEffort    uint64
	CurrentMiner       string
	CurrentMinerEffort uint64
	StartTimestamp     time.Time
}

func (m *Miner) getApplicationData(ctx context.Context) (AppData, error) {
	app, err := m.client.GetApplicationByID(appID).Do(ctx)
	if err != nil {
		return AppData{}, err
	}

	appData := AppData{
		ID: appID,
	}

	for _, gs := range app.Params.GlobalState {
		name, err := base64.StdEncoding.DecodeString(gs.Key)
		if err != nil {
			return AppData{}, err
		}

		switch string(name) {
		case "token":
			appData.Asset = gs.Value.Uint
		case "block":
			appData.Block = gs.Value.Uint
		case "total_effort":
			appData.TotalEffort = gs.Value.Uint
		case "total_transactions":
			appData.TotalTransactions = gs.Value.Uint
		case "halving":
			appData.Halving = gs.Value.Uint
		case "halving_supply":
			appData.HalvingSupply = gs.Value.Uint
		case "mined_supply":
			appData.MinedSupply = gs.Value.Uint
		case "miner_reward":
			appData.MinerReward = gs.Value.Uint
		case "last_miner":
			b, err := base64.StdEncoding.DecodeString(gs.Value.Bytes)
			if err != nil {
				return AppData{}, err
			}

			appData.LastMiner, err = types.EncodeAddress(b)
			if err != nil {
				return AppData{}, err
			}
		case "last_miner_effort":
			appData.LastMinerEffort = gs.Value.Uint
		case "current_miner":
			b, err := base64.StdEncoding.DecodeString(gs.Value.Bytes)
			if err != nil {
				return AppData{}, err
			}

			appData.CurrentMiner, err = types.EncodeAddress(b)
			if err != nil {
				return AppData{}, err
			}
		case "current_miner_effort":
			appData.CurrentMinerEffort = gs.Value.Uint
		case "start_timestamp":
			appData.StartTimestamp = time.Unix(int64(gs.Value.Uint), 0)
		}
	}

	return appData, nil
}

func (m *Miner) waitForStart(ctx context.Context) {
	now := time.Now()
	appData, err := m.getApplicationData(ctx)
	if err != nil {
		slog.Error("failed to get application data", "err", err)
		panic("stopping execution because retreiving application data failed")
	}
	for now.Before(appData.StartTimestamp) {
		slog.Info("waiting for mining to begin", "timeRemaining", appData.StartTimestamp.Sub(now).String())
		time.Sleep(5 * time.Second)
		appData, err = m.getApplicationData(ctx)
		if err != nil {
			slog.Error("failed to get application data", "err", err)
			panic("stopping execution because retreiving application data failed")
		}
		now = time.Now()
	}
}

func (m *Miner) mine(ctx context.Context) {
	intervalSec := 2
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)

	loops := 0

	intervals := 60 / intervalSec
	avgTxnPerInterval := float64(m.tpm) / (float64(60) / float64(intervalSec))
	counter := uint64(0)

	startTime := time.Now()

	m.waitForStart(ctx)

	slog.Info("Starting miner...")
	for {
		if loops%5 == 0 {
			slog.Info("Mining stats", "totalTxns", m.total.Load(), "elapsedTime", time.Now().Sub(startTime).String())
			bal, err := m.getMinerBalance(ctx)
			if err != nil {
				slog.Error("failed to get miner balance, sleeping for 5 seconds and continuing...", "err", err)
				time.Sleep(5 * time.Second)
				continue
			}

			if bal < 0 {
				slog.Error("miner has low balance. exiting...")
				return
			}
		}

		if loops%intervals == 0 {
			counter = 0
		}

		appData, err := m.getApplicationData(ctx)
		if err != nil {
			slog.Error("failed to get application data, sleeping for 5 seconds and continuing...", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}

		sp, err := m.client.SuggestedParams().Do(ctx)
		if err != nil {
			slog.Error("failed to get suggested params, sleeping for 5 seconds and continuing...", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}

		sp.FlatFee = true
		sp.Fee = types.MicroAlgos(m.fee)

		txnsToSend := uint64(avgTxnPerInterval)

		if float64(counter) < float64(loops%intervals)*avgTxnPerInterval {
			txnsToSend += 1
		}

		for j := counter; j < counter+txnsToSend; j += 16 {
			end := min(j+16, counter+txnsToSend)

			go func(appData AppData, sp types.SuggestedParams, start uint64, end uint64) {
				err := m.submitGroup(ctx, appData, sp, start, end)
				if err != nil {
					slog.Error("failed to submit transaction group", "err", err)
				}
			}(appData, sp, j, end)
		}

		loops++
		counter += txnsToSend

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Miner) submitGroup(ctx context.Context, appData AppData, sp types.SuggestedParams, start uint64, end uint64) error {
	composer := transaction.AtomicTransactionComposer{}

	newStart := start

	if m.fee == 1000 && start < end {
		newSp := sp
		newSp.Fee = 2000
		composer.AddMethodCall(transaction.AddMethodCallParams{
			AppID:           m.appID,
			Method:          m.method,
			MethodArgs:      []any{m.depositAddress},
			Sender:          m.minerAccount.Address,
			SuggestedParams: sp,
			Signer:          transaction.BasicAccountTransactionSigner{Account: m.minerAccount},
			ForeignAccounts: []string{appData.LastMiner, m.depositAddress.String()},
			ForeignAssets:   []uint64{appData.Asset},
			Note:            []byte(fmt.Sprint(start)),
		})

		newStart += 1
	}

	for i := newStart; i < end; i++ {
		composer.AddMethodCall(transaction.AddMethodCallParams{
			AppID:           m.appID,
			Method:          m.method,
			MethodArgs:      []any{m.depositAddress},
			Sender:          m.minerAccount.Address,
			SuggestedParams: sp,
			Signer:          transaction.BasicAccountTransactionSigner{Account: m.minerAccount},
			ForeignAccounts: []string{appData.LastMiner, m.depositAddress.String()},
			ForeignAssets:   []uint64{appData.Asset},
			Note:            []byte(fmt.Sprint(i)),
		})
	}

	_, err := composer.Execute(m.client, ctx, 5)
	if err != nil {
		return err
	}

	m.total.Add(end - start)

	return nil
}
