package main

import "github.com/decred/dcrwallet/ticketbuyer"

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

// blockConnectedHandler handles block connected notifications, which trigger
// ticket purchases.
func (p *purchaseManager) blockConnectedHandler() {
out:
	for {
		select {
		case height := <-p.blockConnectedChan:
			log.Infof("Block height %v connected", height)
			ps, err := p.purchaser.Purchase(height)
			if err != nil {
				log.Errorf("Failed to purchase tickets this round: %s",
					err.Error())
			}

			// Write ticket fee info for the current block to the
			// CSV update data.
			err = writeStatsCsvFile(ps.Height, ps.Balance, ps.TicketPrice)
			if err != nil {
				log.Errorf("Failed to write purchase stats: %v", err)
			}
			err = writeToCsvFiles(ps)
			if err != nil {
				log.Errorf("Failed to write CSV graph data: %s", err)
			}

		// TODO Poll every couple minute to check if connected;
		// if not, try to reconnect.
		case <-p.quit:
			break out
		}
	}
}
