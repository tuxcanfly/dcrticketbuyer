package main

import (
	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrticketbuyer/ticketbuyer"
)

// zeroUint32 is the zero value for a uint32.
var zeroUint32 = uint32(0)

// purchaseManager is the main handler of websocket notifications to
// pass to the purchaser and internal quit notifications.
type purchaseManager struct {
	purchaser          *ticketbuyer.TicketPurchaser
	blockConnectedChan chan int64
	quit               chan struct{}
}

// newPurchaseManager creates a new purchaseManager.
func newPurchaseManager(purchaser *ticketbuyer.TicketPurchaser,
	blockConnChan chan int64,
	quit chan struct{}) *purchaseManager {
	return &purchaseManager{
		purchaser:          purchaser,
		blockConnectedChan: blockConnChan,
		quit:               quit,
	}
}

// purchasedFn is a callback invoked after purchase.
type purchasedFn func(pInfo *ticketbuyer.PurchaseInfo)

// blockConnectedHandler handles block connected notifications, which trigger
// ticket purchases.
func (p *purchaseManager) blockConnectedHandler(fn purchasedFn) {
out:
	for {
		select {
		case height := <-p.blockConnectedChan:
			log.Infof("Block height %v connected", height)

			pInfo, err := p.purchaser.Purchase(height)
			if err != nil {
				log.Errorf("Failed to purchase tickets this round: %s",
					err.Error())
				continue
			}
			fn(pInfo)

		// TODO Poll every couple minute to check if connected;
		// if not, try to reconnect.
		case <-p.quit:
			break out
		}
	}
}

func writePurchaseInfo(pInfo *ticketbuyer.PurchaseInfo, dcrdChainSvr *dcrrpcclient.Client) {
	// Initialize webserver update data.
	var csvData csvUpdateData

	// Write ticket fee info for the current block to the
	// CSV update data.
	oneBlock := uint32(1)
	info, err := dcrdChainSvr.TicketFeeInfo(&oneBlock, &zeroUint32)
	if err != nil {
		log.Errorf("Failed to fetch all mempool tickets: %s",
			err.Error())
		return
	}
	csvData.tfMin = info.FeeInfoBlocks[0].Min
	csvData.tfMax = info.FeeInfoBlocks[0].Max
	csvData.tfMedian = info.FeeInfoBlocks[0].Median
	csvData.tfMean = info.FeeInfoBlocks[0].Mean

	// The expensive call to fetch all tickets in the mempool
	// is here.
	tfi, err := dcrdChainSvr.TicketFeeInfo(&zeroUint32, &zeroUint32)
	if err != nil {
		log.Errorf("Failed to fetch all mempool tickets: %s",
			err.Error())
		return
	}

	all := int(tfi.FeeInfoMempool.Number)
	csvData.tnAll = all

	csvData.height = pInfo.Height
	csvData.tpAverage = pInfo.Average
	csvData.tpCurrent = pInfo.Current
	csvData.tpNext = pInfo.Next
	csvData.tpMaxScale = pInfo.MaxScale
	csvData.tpMinScale = pInfo.MinScale
	csvData.leftWindow = pInfo.LeftWindow
	csvData.tnOwn = pInfo.TnOwn
	csvData.tfOwn = pInfo.TfOwn
	csvData.purchased = pInfo.Purchased

	//  Defer a function that writes this data to the
	//  disk.
	defer func() {
		err = writeToCsvFiles(csvData)
		if err != nil {
			log.Errorf("Failed to write CSV graph data: %s",
				err.Error())
			return
		}
	}()
}
