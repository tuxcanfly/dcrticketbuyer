// Copyright (c) 2016 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrticketbuyer/ticketbuyer"
	"github.com/decred/dcrutil"
)

// Global variables are displayed below that are mainly used for the purposes
// of the HTTP server.
var (
	// chainHeight is the global chainHeight. It must be accessed with
	// atomic operators.
	glChainHeight = int64(0)

	// glBalance is the global balance. It is updated at start up and
	// after every round of ticket purchases. It must be accessed with
	// atomic operators.
	glBalance = int64(0)

	// glTicketPrice is the global ticket price. It is updated at
	// start up and after every round of ticket purchases.
	glTicketPrice = int64(0)
)

const (
	// blockConnChanBuffer is the size of the block connected channel buffer.
	blockConnChanBuffer = 100
)

// syncGlobalsStartup syncs the globals for the HTTP server on startup.
func syncGlobalsStartup(dcrdClient *dcrrpcclient.Client,
	dcrwClient *dcrrpcclient.Client, cfg *config) error {
	_, height, err := dcrdClient.GetBestBlock()
	if err != nil {
		return err
	}
	atomic.StoreInt64(&glChainHeight, height)

	bal, err := dcrwClient.GetBalanceMinConfType(cfg.AccountName,
		0, "spendable")
	if err != nil {
		return err
	}
	atomic.StoreInt64(&glBalance, int64(bal))

	sd, err := dcrdClient.GetStakeDifficulty()
	if err != nil {
		return err
	}
	nsdAmt, err := dcrutil.NewAmount(sd.NextStakeDifficulty)
	if err != nil {
		return err
	}
	atomic.StoreInt64(&glTicketPrice, int64(nsdAmt))

	return nil
}

func main() {
	// Parse the configuration file.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Failed to load ticketbuyer config: %s\n", err.Error())
		os.Exit(1)
	}
	defer backendLog.Flush()

	dcrrpcclient.UseLogger(clientLog)

	// Initialize the CSV files for the HTTP server if needed.
	if cfg.HTTPSvrPort != 0 {
		err := initCsvFiles()
		if err != nil {
			fmt.Printf("Failed to init https files: %s\n", err.Error())
			os.Exit(1)
		}
	}

	// Connect to dcrd RPC server using websockets. Set up the
	// notification handler to deliver blocks through a channel.
	connectChan := make(chan int64, blockConnChanBuffer)
	quit := make(chan struct{})
	ntfnHandlersDaemon := dcrrpcclient.NotificationHandlers{
		OnBlockConnected: func(serializedBlockHeader []byte, transactions [][]byte) {
			var blockHeader wire.BlockHeader
			err := blockHeader.Deserialize(bytes.NewReader(serializedBlockHeader))
			if err != nil {
				log.Errorf("Failed to deserialize block header: %v", err.Error())
				return
			}
			connectChan <- int64(blockHeader.Height)
		},
	}

	var dcrdCerts []byte
	if !cfg.DisableClientTLS {
		dcrdCerts, err = ioutil.ReadFile(cfg.DcrdCert)
		if err != nil {
			fmt.Printf("Failed to read dcrd cert file at %s: %s\n", cfg.DcrdCert,
				err.Error())
			os.Exit(1)
		}
	}
	tkbyLog.Debugf("Attempting to connect to dcrd RPC %s as user %s "+
		"using certificate located in %s",
		cfg.DcrdServ, cfg.DcrdUser, cfg.DcrdCert)
	connCfgDaemon := &dcrrpcclient.ConnConfig{
		Host:         cfg.DcrdServ,
		Endpoint:     "ws",
		User:         cfg.DcrdUser,
		Pass:         cfg.DcrdPass,
		Certificates: dcrdCerts,
		DisableTLS:   cfg.DisableClientTLS,
	}
	dcrdClient, err := dcrrpcclient.New(connCfgDaemon, &ntfnHandlersDaemon)
	if err != nil {
		fmt.Printf("Failed to start dcrd rpcclient: %s\n", err.Error())
		os.Exit(1)
	}

	// Register for block connection notifications.
	if err := dcrdClient.NotifyBlocks(); err != nil {
		fmt.Printf("Failed to start register daemon rpc client for  "+
			"block notifications: %s\n", err.Error())
		os.Exit(1)
	}

	// Connect to the dcrwallet server RPC client.
	var dcrwCerts []byte
	if !cfg.DisableClientTLS {
		dcrwCerts, err = ioutil.ReadFile(cfg.DcrwCert)
		if err != nil {
			fmt.Printf("Failed to read dcrwallet cert file at %s: %s\n",
				cfg.DcrwCert, err.Error())
		}
	}
	connCfgWallet := &dcrrpcclient.ConnConfig{
		Host:         cfg.DcrwServ,
		Endpoint:     "ws",
		User:         cfg.DcrwUser,
		Pass:         cfg.DcrwPass,
		Certificates: dcrwCerts,
		DisableTLS:   cfg.DisableClientTLS,
	}
	tkbyLog.Debugf("Attempting to connect to dcrwallet RPC %s as user %s "+
		"using certificate located in %s",
		cfg.DcrwServ, cfg.DcrwUser, cfg.DcrwCert)
	dcrwClient, err := dcrrpcclient.New(connCfgWallet, nil)
	if err != nil {
		fmt.Printf("Failed to start dcrw rpcclient: %s\n", err.Error())
		os.Exit(1)
	}

	wi, err := dcrwClient.WalletInfo()
	if err != nil {
		fmt.Printf("Failed to get WalletInfo on start: %s\n", err.Error())
		os.Exit(1)
	}
	if !wi.DaemonConnected {
		tkbyLog.Warnf("Wallet was not connected to a daemon at start up! " +
			"Please ensure wallet has proper connectivity.")
	}
	if !wi.Unlocked {
		tkbyLog.Warnf("Wallet is not unlocked! You will need to unlock " +
			"wallet for tickets to be purchased.")
	}

	err = syncGlobalsStartup(dcrdClient, dcrwClient, cfg)
	if err != nil {
		fmt.Printf("Failed to start sync globals on startup: %s\n", err.Error())
		os.Exit(1)
	}

	// Ctrl-C to kill.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			quit <- struct{}{}
		}
	}()

	var prevToBuyDiffPeriod, prevToBuyHeight int
	// Here we attempt to load purchased.csv from the webui dir.  This
	// allows us to attempt to see if there have been previous ticketbuyer
	// instances during the current stakediff window and reuse the
	// previously tracked amount of tickets to purchase during that window.
	f, err := os.OpenFile(filepath.Join(csvPath, csvPurchasedFn),
		os.O_RDONLY|os.O_CREATE, 0600)
	if err != nil {
		fmt.Printf("Error opening file: %v", err)
		os.Exit(1)
	}

	rdr := csv.NewReader(f)
	rdr.Comma = ','
	prevRecord := []string{}
	for {
		record, err := rdr.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
		}
		prevRecord = record
	}
	f.Close()

	// Attempt to parse last line in purchased.csv, then set previous amounts.
	if len(prevRecord) >= 3 {
		if prevRecord[1] == "RemainingToBuy" {
			prevToBuyHeight, err = strconv.Atoi(prevRecord[0])
			if err != nil {
				log.Errorf("Could not parse last height from "+
					"csv: %v", err)
			}

			prevToBuyDiffPeriod, err = strconv.Atoi(prevRecord[2])
			if err != nil {
				log.Errorf("Could not parse remaining to buy "+
					"from csv %v", err)
			}
		}
	}

	ticketbuyerCfg := &ticketbuyer.Config{
		AccountName:         cfg.AccountName,
		AvgPriceMode:        cfg.AvgPriceMode,
		AvgPriceVWAPDelta:   cfg.AvgPriceVWAPDelta,
		BalanceToMaintain:   cfg.BalanceToMaintain,
		BlocksToAvg:         cfg.BlocksToAvg,
		DontWaitForTickets:  cfg.DontWaitForTickets,
		ExpiryDelta:         cfg.ExpiryDelta,
		FeeSource:           cfg.FeeSource,
		FeeTargetScaling:    cfg.FeeTargetScaling,
		HighPricePenalty:    cfg.HighPricePenalty,
		MinFee:              cfg.MinFee,
		MinPriceScale:       cfg.MinPriceScale,
		MaxFee:              cfg.MaxFee,
		MaxPerBlock:         cfg.MaxPerBlock,
		MaxPriceAbsolute:    cfg.MaxPriceAbsolute,
		MaxPriceScale:       cfg.MaxPriceScale,
		MaxInMempool:        cfg.MaxInMempool,
		PoolAddress:         cfg.PoolAddress,
		PoolFees:            cfg.PoolFees,
		PriceTarget:         cfg.PriceTarget,
		TicketAddress:       cfg.TicketAddress,
		TxFee:               cfg.TxFee,
		PrevToBuyDiffPeriod: prevToBuyDiffPeriod,
		PrevToBuyHeight:     prevToBuyHeight,
	}
	purchaser, err := ticketbuyer.NewTicketPurchaser(ticketbuyerCfg,
		dcrdClient, dcrwClient, activeNet.Params)
	if err != nil {
		fmt.Printf("Failed to start purchaser: %s\n", err.Error())
		os.Exit(1)
	}

	wsm := newPurchaseManager(purchaser, connectChan, quit)
	go wsm.blockConnectedHandler()

	tkbyLog.Infof("Daemon and wallet successfully connected, beginning " +
		"to purchase tickets")

	err = purchaser.Purchase(atomic.LoadInt64(&glChainHeight))
	if err != nil {
		tkbyLog.Errorf("Failed to purchase tickets this round: %s",
			err.Error())
	}

	// If the HTTP server is enabled, spin it up and begin
	// displaying the front page locally.
	if cfg.HTTPSvrPort > 0 {
		go func() {
			port := strconv.Itoa(cfg.HTTPSvrPort)
			http.HandleFunc("/", writeMainGraphs)
			http.Handle("/csvdata/", http.StripPrefix("/csvdata/", http.FileServer(http.Dir(cfg.DataDir))))
			err := http.ListenAndServe(cfg.HTTPSvrBind+":"+port, nil)
			if err != nil {
				tkbyLog.Errorf("Failed to bind http server: %s", err.Error())
			}
		}()
	}

	<-quit
	close(quit)
	dcrdClient.Disconnect()
	dcrwClient.Disconnect()
	fmt.Printf("\nClosing ticket buyer.\n")
	os.Exit(1)
}
