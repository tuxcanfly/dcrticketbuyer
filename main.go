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

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrutil"
	"github.com/decred/dcrwallet/ticketbuyer"
)

const (
	// blockConnChanBuffer is the size of the block connected channel buffer.
	blockConnChanBuffer = 100
)

// writeStats writes the stats to a CSV file for use by the HTTP server on
// startup.
func writeStats(dcrdClient *dcrrpcclient.Client, dcrwClient *dcrrpcclient.Client,
	cfg *config) error {
	_, height, err := dcrdClient.GetBestBlock()
	if err != nil {
		return err
	}

	bal, err := dcrwClient.GetBalanceMinConfType(cfg.AccountName,
		0, "spendable")
	if err != nil {
		return err
	}

	sd, err := dcrdClient.GetStakeDifficulty()
	if err != nil {
		return err
	}
	nsdAmt, err := dcrutil.NewAmount(sd.NextStakeDifficulty)
	if err != nil {
		return err
	}
	return writeStatsCsvFile(height, int64(bal), int64(nsdAmt))
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
	log.Debugf("Attempting to connect to dcrd RPC %s as user %s "+
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
	log.Debugf("Attempting to connect to dcrwallet RPC %s as user %s "+
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
		log.Warnf("Wallet was not connected to a daemon at start up! " +
			"Please ensure wallet has proper connectivity.")
	}
	if !wi.Unlocked {
		log.Warnf("Wallet is not unlocked! You will need to unlock " +
			"wallet for tickets to be purchased.")
	}

	err = writeStats(dcrdClient, dcrwClient, cfg)
	if err != nil {
		fmt.Printf("Failed to write stats on startup: %v\n", err)
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
		TicketFeeInfo:       true,
		PrevToBuyDiffPeriod: prevToBuyDiffPeriod,
		PrevToBuyHeight:     prevToBuyHeight,
	}
	walletCfg := &ticketbuyer.WalletCfg{
		GetOwnMempoolTix: func() (uint32, error) {
			sinfo, err := dcrwClient.GetStakeInfo()
			if err != nil {
				return 0, err
			}
			return sinfo.OwnMempoolTix, nil
		},
		SetTxFee: func(fee dcrutil.Amount) error {
			return dcrwClient.SetTxFee(fee)
		},
		SetTicketFee: func(fee dcrutil.Amount) error {
			return dcrwClient.SetTicketFee(fee)
		},
		GetBalance: func() (dcrutil.Amount, error) {
			return dcrwClient.GetBalanceMinConfType(cfg.AccountName, 0, "spendable")
		},
		GetRawChangeAddress: func() (dcrutil.Address, error) {
			return dcrwClient.GetRawChangeAddress(cfg.AccountName)
		},
		PurchaseTicket: func(
			spendLimit dcrutil.Amount,
			minConf *int,
			ticketAddress dcrutil.Address,
			numTickets *int,
			poolAddress dcrutil.Address,
			poolFees *dcrutil.Amount,
			expiry *int) ([]*chainhash.Hash, error) {
			return dcrwClient.PurchaseTicket(
				cfg.AccountName,
				spendLimit,
				minConf,
				ticketAddress,
				numTickets,
				poolAddress,
				poolFees,
				expiry,
			)
		},
	}
	purchaser, err := ticketbuyer.NewTicketPurchaser(ticketbuyerCfg,
		dcrdClient, walletCfg, activeNet.Params)
	if err != nil {
		fmt.Printf("Failed to start purchaser: %s\n", err.Error())
		os.Exit(1)
	}

	wsm := newPurchaseManager(purchaser, connectChan, quit)
	go wsm.blockConnectedHandler()

	log.Infof("Daemon and wallet successfully connected, beginning " +
		"to purchase tickets")

	// If the HTTP server is enabled, spin it up and begin
	// displaying the front page locally.
	if cfg.HTTPSvrPort > 0 {
		go func() {
			port := strconv.Itoa(cfg.HTTPSvrPort)
			http.HandleFunc("/", writeMainGraphs)
			http.Handle("/csvdata/", http.StripPrefix("/csvdata/", http.FileServer(http.Dir(cfg.DataDir))))
			err := http.ListenAndServe(cfg.HTTPSvrBind+":"+port, nil)
			if err != nil {
				log.Errorf("Failed to bind http server: %s", err.Error())
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
