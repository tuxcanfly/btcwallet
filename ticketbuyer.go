package main

import (
	"io/ioutil"

	"github.com/decred/dcrrpcclient"
	"github.com/decred/dcrticketbuyer/ticketbuyer"
	"github.com/decred/dcrwallet/wallet"
)

func getWalletRPCClient() (*dcrrpcclient.Client, error) {
	// Connect to the dcrwallet server RPC client.
	var dcrwCerts []byte
	if !cfg.DisableClientTLS {
		var err error
		dcrwCerts, err = ioutil.ReadFile(cfg.RPCCert)
		if err != nil {
			return nil, err
		}
	}
	connCfgWallet := &dcrrpcclient.ConnConfig{
		Host:         "localhost:19110", // TODO: Use cfg.ExperimentalRPCListeners/LegacyRPCListeners
		Endpoint:     "ws",
		User:         cfg.Username,
		Pass:         cfg.Password,
		Certificates: dcrwCerts,
		DisableTLS:   cfg.DisableClientTLS,
	}
	tkbyLog.Debugf("Attempting to connect to dcrwallet RPC %s as user %s "+
		"using certificate located in %s",
		"localhost", cfg.Username, cfg.Password)
	dcrwClient, err := dcrrpcclient.New(connCfgWallet, nil)
	if err != nil {
		return nil, err
	}

	wi, err := dcrwClient.WalletInfo()
	if err != nil {
		return nil, err
	}
	if !wi.DaemonConnected {
		tkbyLog.Warnf("Wallet was not connected to a daemon at start up! " +
			"Please ensure wallet has proper connectivity.")
	}
	if !wi.Unlocked {
		tkbyLog.Warnf("Wallet is not unlocked! You will need to unlock " +
			"wallet for tickets to be purchased.")
	}
	return dcrwClient, nil
}

func startTicketPurchase(w *wallet.Wallet) {
	dcrwClient, err := getWalletRPCClient()
	if err != nil {
		log.Errorf("Error fetching wallet rpc client: %v", err)
		return
	}
	dcrdClient := w.ChainClient().Client
	purchaser, err := ticketbuyer.NewTicketPurchaser(&ticketbuyer.Config{}, dcrdClient, dcrwClient, activeNet.Params)
	if err != nil {
		log.Errorf("Error starting ticketbuyer: %v", err)
		return
	}
	w.SetTicketPurchaser(purchaser)
}