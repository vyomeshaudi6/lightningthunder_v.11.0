// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package lnd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof" // Blank import to set up profiling HTTP handlers.
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wallet"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/neutrino"
	"golang.org/x/crypto/acme/autocert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"

	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)
var (
	
	registeredChains = newChainRegistry()

	//ChanDB *channeldb.DB // channel.db
	LocalChanDB  *channeldb.DB
	RemoteChanDB  *channeldb.DB
	// networkDir is the path to the directory of the currently active
	// network. This path will hold the files related to each different
	// network.
	networkDir         string
	RpcserverInstances []*rpcServer
	UserId             string // added userid for multiple server instances and passed to new server func
	Cfg		   *Config
	Controller_Config  *Config
	rpcPortListening   string
	restPortListening  string
	peerPortListening  string
	Loader		  *wallet.Loader  
)
// WalletUnlockerAuthOptions returns a list of DialOptions that can be used to
// authenticate with the wallet unlocker service.
//
// NOTE: This should only be called after the WalletUnlocker listener has
// signaled it is ready.
func WalletUnlockerAuthOptions(Cfg *Config) ([]grpc.DialOption, error) {
	creds, err := credentials.NewClientTLSFromFile(Cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array with the TLS credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	return opts, nil
}

// AdminAuthOptions returns a list of DialOptions that can be used to
// authenticate with the RPC server with admin capabilities.
//
// NOTE: This should only be called after the RPCListener has signaled it is
// ready.
func AdminAuthOptions(Cfg *Config) ([]grpc.DialOption, error) {
	creds, err := credentials.NewClientTLSFromFile(Cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Get the admin macaroon if macaroons are active.
	if !Cfg.NoMacaroons {
		// Load the adming macaroon file.
		macBytes, err := ioutil.ReadFile(Cfg.AdminMacPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read macaroon "+
				"path (check the network setting!): %v", err)
		}

		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			return nil, fmt.Errorf("unable to decode macaroon: %v",
				err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred := macaroons.NewMacaroonCredential(mac)
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	return opts, nil
}

// GrpcRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own gRPC server onto lnd's main
// grpc.Server instance.
type GrpcRegistrar interface {
	// RegisterGrpcSubserver is called for each net.Listener on which lnd
	// creates a grpc.Server instance. External subservers implementing this
	// method can then register their own gRPC server structs to the main
	// server instance.
	RegisterGrpcSubserver(*grpc.Server) error
}

// RestRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own REST mux onto lnd's main
// proxy.ServeMux instance.
type RestRegistrar interface {
	// RegisterRestSubserver is called after lnd creates the main
	// proxy.ServeMux instance. External subservers implementing this method
	// can then register their own REST proxy stubs to the main server
	// instance.
	RegisterRestSubserver(context.Context, *proxy.ServeMux, string,
		[]grpc.DialOption) error
}

// RPCSubserverConfig is a struct that can be used to register an external
// subserver with the custom permissions that map to the gRPC server that is
// going to be registered with the GrpcRegistrar.
type RPCSubserverConfig struct {
	// Registrar is a callback that is invoked for each net.Listener on
	// which lnd creates a grpc.Server instance.
	Registrar GrpcRegistrar

	// Permissions is the permissions required for the external subserver.
	// It is a map between the full HTTP URI of each RPC and its required
	// macaroon permissions. If multiple action/entity tuples are specified
	// per URI, they are all required. See rpcserver.go for a list of valid
	// action and entity values.
	Permissions map[string][]bakery.Op

	// MacaroonValidator is a custom macaroon validator that should be used
	// instead of the default lnd validator. If specified, the custom
	// validator is used for all URIs specified in the above Permissions
	// map.
	MacaroonValidator macaroons.MacaroonValidator
}

// ListenerWithSignal is a net.Listener that has an additional Ready channel that
// will be closed when a server starts listening.
type ListenerWithSignal struct {
	net.Listener

	// Ready will be closed by the server listening on Listener.
	Ready chan struct{}

	// ExternalRPCSubserverCfg is optional and specifies the registration
	// callback and permissions to register external gRPC subservers.
	ExternalRPCSubserverCfg *RPCSubserverConfig

	// ExternalRestRegistrar is optional and specifies the registration
	// callback to register external REST subservers.
	ExternalRestRegistrar RestRegistrar
}

// ListenerCfg is a wrapper around custom listeners that can be passed to lnd
// when calling its main method.
type ListenerCfg struct {
	// WalletUnlocker can be set to the listener to use for the wallet
	// unlocker. If nil a regular network listener will be created.
	WalletUnlocker *ListenerWithSignal

	// RPCListener can be set to the listener to use for the RPC server. If
	// nil a regular network listener will be created.
	RPCListener *ListenerWithSignal
}

// rpcListeners is a function type used for closures that fetches a set of RPC
// listeners for the current configuration. If no custom listeners are present,
// this should return normal listeners from the RPC endpoints defined in the
// config. The second return value us a closure that will close the fetched
// listeners.
type rpcListeners func() ([]*ListenerWithSignal, func(), error)

// Main is the true entry point for lnd. It accepts a fully populated and
// validated main configuration struct and an optional listener config struct.
// This function starts all main system components then blocks until a signal
// is received on the shutdownChan at which point everything is shut down again.
func Main( lisCfg ListenerCfg, shutdownChan <-chan struct{}) error {
	// Hook interceptor for os signals.
	signal.Intercept()	
	// Load the configuration, and parse any command line options. This
	// function will also set up logging properly.
	loadedConfig, err := LoadConfig("")
	if err != nil {
		return err
	}
	Cfg = loadedConfig
	Controller_Config = loadedConfig	
	defer func() {
		ltndLog.Info("Shutdown complete")
		err := Controller_Config.LogWriter.Close()
		if err != nil {
			ltndLog.Errorf("Could not close log rotator: %v", err)
		}
	}()

	// Show version at startup.
	ltndLog.Infof("Version: %s commit=%s, build=%s, logging=%s",
		build.Version(), build.Commit, build.Deployment,
		build.LoggingType)

	var network string
	switch {
	case Controller_Config.Bitcoin.TestNet3 || Controller_Config.Litecoin.TestNet3:
		network = "testnet"

	case Controller_Config.Bitcoin.MainNet || Controller_Config.Litecoin.MainNet:
		network = "mainnet"

	case Controller_Config.Bitcoin.SimNet || Controller_Config.Litecoin.SimNet:
		network = "simnet"

	case Controller_Config.Bitcoin.RegTest || Controller_Config.Litecoin.RegTest:
		network = "regtest"
	}

	ltndLog.Infof("Active chain: %v (network=%v)",
		strings.Title(Controller_Config.registeredChains.PrimaryChain().String()),
		network,
	)

	// Enable http profiling server if requested.
	if Controller_Config.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", Controller_Config.Profile)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Write cpu profile if requested.
	if Controller_Config.CPUProfile != "" {
		f, err := os.Create(Controller_Config.CPUProfile)
		if err != nil {
			err := fmt.Errorf("unable to create CPU profile: %v",
				err)
			ltndLog.Error(err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	
	// Only process macaroons if --no-macaroons isn't set.
	tlsCfg, restCreds,cleanUp, err := getTLSConfig(Controller_Config)
	if err != nil {
		err := fmt.Errorf("unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer cleanUp()
	serverCreds := credentials.NewTLS(tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// For our REST dial options, we'll still use TLS, but also increase
	// the max message size that we'll decode to allow clients to hit
	// endpoints which return more data such as the DescribeGraph call.
	// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
	// in cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(*restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200),
		),
	}

	// Before starting the wallet, we'll create and start our Neutrino
	// light client instance, if enabled, in order to allow it to sync
	// while the rest of the daemon continues startup.
	mainChain := Controller_Config.Bitcoin
	if Controller_Config.registeredChains.PrimaryChain() == litecoinChain {
		mainChain = Controller_Config.Litecoin
	}
	var neutrinoCS *neutrino.ChainService
	if mainChain.Node == "neutrino" {
		neutrinoBackend, neutrinoCleanUp, err := initNeutrinoBackend(
			Controller_Config, mainChain.ChainDir,
		)
		if err != nil {
			err := fmt.Errorf("unable to initialize neutrino "+
				"backend: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer neutrinoCleanUp()
		neutrinoCS = neutrinoBackend
	}

	var (
		walletInitParams WalletUnlockParams
		privateWalletPw  = lnwallet.DefaultPrivatePassphrase
		publicWalletPw   = lnwallet.DefaultPublicPassphrase
	)


	// getListeners is a closure that creates listeners from the
	// RPCListeners defined in the config. It also returns a cleanup
	// closure and the server options to use for the GRPC server.
	getListeners := func(callerservice string) ([]*ListenerWithSignal, func(), error) {
		var grpcListeners []*ListenerWithSignal
		//modified added caller service check in order to reserve rpc 1st port for wallet unlocker service and remaining for lightning service
		if callerservice == "walletaction" {
			for _, grpcEndpoint := range Controller_Config.RPCListeners {
				// Start a gRPC server listening for HTTP/2
				// connections.
				lis, err := lncfg.ListenOnAddress(grpcEndpoint)
				if err != nil {
					//ltndLog.Errorf("unable to listen on %s",
					//	grpcEndpoint)
					continue
					//return nil, nil, nil, err
				}
				//grpcListeners = append(grpcListeners, lis)
				grpcListeners = append(
					grpcListeners, &ListenerWithSignal{
						Listener: lis,
						Ready:    make(chan struct{}),
					})
				break
			}
		} else {
			for _, grpcEndpoint := range Cfg.RPCListeners {
				// Start a gRPC server listening for HTTP/2
				// connections.
				//skipped the first iteration inorder to save the first rpc port for walletunlocker service
				//if i == 0 {
				//	continue
				//}
				lis, err := lncfg.ListenOnAddress(grpcEndpoint)
				if err != nil {
					//ltndLog.Errorf("unable to listen on %s",
					//	grpcEndpoint)
					continue
					//return nil, nil, nil, err
				}
				//grpcListeners = append(grpcListeners, lis)
				grpcListeners = append(
					grpcListeners, &ListenerWithSignal{
						Listener: lis,
						Ready:    make(chan struct{}),
					})
				break
			}
		}
		cleanup := func() {
			for _, lis := range grpcListeners {
				lis.Close()
			}
		}
		return grpcListeners, cleanup, nil
	}


	// walletUnlockerListeners is a closure we'll hand to the wallet
	// unlocker, that will be called when it needs listeners for its GPRC
	// server.
	walletUnlockerListeners := func() ([]*ListenerWithSignal, func(),
		error) {

		// If we have chosen to start with a dedicated listener for the
		// wallet unlocker, we return it directly.
		if lisCfg.WalletUnlocker != nil {
			return []*ListenerWithSignal{lisCfg.WalletUnlocker},
				func() {}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners("walletaction")
	}

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noseedbackup flag, we use the default password
	// for wallet encryption.
	// for loop edit
	for i := 0; i < 100; i++ {
	// If the user didn't request a seed, then we'll manually assume a
		// wallet birthday of now, as otherwise the seed would've specified
		// this information.
		walletInitParams.Birthday = time.Now()
			// code modified to get rest proxy set accoring to rpc port i.e linking rpc port and rest proxy port together
		// restproxy des modified linkednd 1st rpc port and 1st rest port for wallet unlocker service
		restProxyDest := Controller_Config.RPCListeners[0].String()//rest port for unlock/create from controler config file in .lnd folder
		switch {
		case strings.Contains(restProxyDest, "0.0.0.0"):
			restProxyDest = strings.Replace(
				restProxyDest, "0.0.0.0", "127.0.0.1", 1,
			)

		case strings.Contains(restProxyDest, "[::]"):
			restProxyDest = strings.Replace(
				restProxyDest, "[::]", "[::1]", 1,
			)
		}
		
	if !Controller_Config.NoSeedBackup {
		params, err := waitForWalletPassword(
			Cfg, Controller_Config.RESTListeners, serverOpts, restDialOpts,
			restProxyDest, tlsCfg, walletUnlockerListeners,
		)
		if err != nil {
			err := fmt.Errorf("unable to set up wallet password "+
				"listeners: %v", err)
			ltndLog.Error(err)
			return err
		}

		walletInitParams = *params
		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password
		defer func() {
			if err := walletInitParams.UnloadWallet(); err != nil {
				ltndLog.Errorf("Could not unload wallet: %v", err)
			}
		}()

		if walletInitParams.RecoveryWindow > 0 {
			ltndLog.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				walletInitParams.RecoveryWindow)
		}
	}
		//custom tls code edit 
		Cfg.TLSCertPath = filepath.Join(Cfg.graphDir, DefaultTLSCertFilename)
		Cfg.TLSKeyPath = filepath.Join(Cfg.graphDir, DefaultTLSKeyFilename)
		tlsCfg, restCreds,cleanUp, err := getTLSConfig(Cfg)
		if err != nil {
		err = fmt.Errorf("unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
		}
		defer cleanUp()
		serverCreds := credentials.NewTLS(tlsCfg)
		serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}
	
		// For our REST dial options, we'll still use TLS, but also increase
		// the max message size that we'll decode to allow clients to hit
		// endpoints which return more data such as the DescribeGraph call.
		// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
		// in cmd/lncli/main.go.
		restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(*restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200),
		),
		}

		//defer ChanDB.Close() // channel.db
		defer LocalChanDB.Close() //channeldb 
		defer RemoteChanDB.Close() //channeldb
		
		//walletaction response port for each node
		// rpcPortListening = Cfg.RPCListeners[i+1].String()
		// restPortListening = Cfg.RESTListeners[i+1].String()
		// peerPortListening = Cfg.Listeners[i].String()
  		 
		// restproxy des modified linked 2nd rpc port and 1nd rest port for lightning service
		// restProxyDest = Cfg.RPCListeners[i+1].String()
		
		//--code edit  giving custom macaroon path according to the user id for storing macaroon files of each respective node into their current directory
		Cfg.AdminMacPath = filepath.Join(
			Cfg.graphDir, DefaultAdminMacFilename,
		)
		Cfg.ReadMacPath = filepath.Join(
			Cfg.graphDir, DefaultReadMacFilename,
		)
		Cfg.InvoiceMacPath = filepath.Join(
			Cfg.graphDir, DefaultInvoiceMacFilename,
		)
		//--code edit  giving custom channelbackup  path according to the user id for storing backup files of each respective node into their current directory
		Cfg.BackupFilePath = filepath.Join(
			Cfg.graphDir, DefaultBackupFileName,
		)

	var macaroonService *macaroons.Service
	if !Cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		macaroonService, err = macaroons.NewService(
			Cfg.graphDir, "lnd", macaroons.IPLockChecker,
		)
		if err != nil {
			err := fmt.Errorf("unable to set up macaroon "+
				"authentication: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer macaroonService.Close()

		// Try to unlock the macaroon store with the private password.
		err = macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil {
			err := fmt.Errorf("unable to unlock macaroons: %v", err)
			ltndLog.Error(err)
			return err
		}

		// Create macaroon files for lncli to use if they don't exist.
		if !fileExists(Cfg.AdminMacPath) && !fileExists(Cfg.ReadMacPath) &&
			!fileExists(Cfg.InvoiceMacPath) {

			err = genMacaroons(
				ctx, macaroonService, Cfg.AdminMacPath,
				Cfg.ReadMacPath, Cfg.InvoiceMacPath,
			)
			if err != nil {
				err := fmt.Errorf("unable to create macaroons "+
					"%v", err)
				ltndLog.Error(err)
				return err
			}
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	//
	// When we create the chain control, we need storage for the height
	// hints and also the wallet itself, for these two we want them to be
	// replicated, so we'll pass in the remote channel DB instance.
	activeChainControl, err := newChainControlFromConfig(
		Cfg, LocalChanDB, RemoteChanDB, privateWalletPw, publicWalletPw,
		walletInitParams.Birthday, walletInitParams.RecoveryWindow,
		walletInitParams.Wallet, neutrinoCS,
	)
	if err != nil {
		err := fmt.Errorf("unable to create chain control: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := Cfg.registeredChains.PrimaryChain()
	Cfg.registeredChains.RegisterChain(primaryChain, activeChainControl)

	// TODO(roasbeef): add rotation
	idKeyDesc, err := activeChainControl.keyRing.DeriveKey(
		keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	)
	if err != nil {
		err := fmt.Errorf("error deriving node key: %v", err)
		ltndLog.Error(err)
		return err
	}

	if Cfg.Tor.Active {
		srvrLog.Infof("Proxying all network traffic via Tor "+
			"(stream_isolation=%v)! NOTE: Ensure the backend node "+
			"is proxying over Tor as well", Cfg.Tor.StreamIsolation)
	}

	// If the watchtower client should be active, open the client database.
	// This is done here so that Close always executes when lndMain returns.
	var towerClientDB *wtdb.ClientDB
	if Cfg.WtClient.Active {
		var err error
		towerClientDB, err = wtdb.OpenClientDB(Cfg.localDatabaseDir(UserId))
		if err != nil {
			err := fmt.Errorf("unable to open watchtower client "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerClientDB.Close()
	}

	// If tor is active and either v2 or v3 onion services have been specified,
	// make a tor controller and pass it into both the watchtower server and
	// the regular lnd server.
	var torController *tor.Controller
	if Cfg.Tor.Active && (Cfg.Tor.V2 || Cfg.Tor.V3) {
		torController = tor.NewController(
			Cfg.Tor.Control, Cfg.Tor.TargetIPAddress, Cfg.Tor.Password,
		)

		// Start the tor controller before giving it to any other subsystems.
		if err := torController.Start(); err != nil {
			err := fmt.Errorf("unable to initialize tor controller: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer func() {
			if err := torController.Stop(); err != nil {
				ltndLog.Errorf("error stopping tor controller: %v", err)
			}
		}()
	}

	var tower *watchtower.Standalone
	if Cfg.Watchtower.Active {
		// Segment the watchtower directory by chain and network.
		towerDBDir := filepath.Join(
			Cfg.Watchtower.TowerDir,
			Cfg.registeredChains.PrimaryChain().String(),
			normalizeNetwork(Cfg.ActiveNetParams.Name),
		)

		towerDB, err := wtdb.OpenTowerDB(towerDBDir)
		if err != nil {
			err := fmt.Errorf("unable to open watchtower "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerDB.Close()

		towerKeyDesc, err := activeChainControl.keyRing.DeriveKey(
			keychain.KeyLocator{
				Family: keychain.KeyFamilyTowerID,
				Index:  0,
			},
		)
		if err != nil {
			err := fmt.Errorf("error deriving tower key: %v", err)
			ltndLog.Error(err)
			return err
		}

		wtCfg := &watchtower.Config{
			BlockFetcher:   activeChainControl.chainIO,
			DB:             towerDB,
			EpochRegistrar: activeChainControl.chainNotifier,
			Net:            Cfg.net,
			NewAddress: func() (btcutil.Address, error) {
				return activeChainControl.wallet.NewAddress(
					lnwallet.WitnessPubKey, false,
				)
			},
			NodeKeyECDH: keychain.NewPubKeyECDH(
				towerKeyDesc, activeChainControl.keyRing,
			),
			PublishTx: activeChainControl.wallet.PublishTransaction,
			ChainHash: *Cfg.ActiveNetParams.GenesisHash,
		}

		// If there is a tor controller (user wants auto hidden services), then
		// store a pointer in the watchtower config.
		if torController != nil {
			wtCfg.TorController = torController
			wtCfg.WatchtowerKeyPath = Cfg.Tor.WatchtowerKeyPath

			switch {
			case Cfg.Tor.V2:
				wtCfg.Type = tor.V2
			case Cfg.Tor.V3:
				wtCfg.Type = tor.V3
			}
		}

		wtConfig, err := Cfg.Watchtower.Apply(wtCfg, lncfg.NormalizeAddresses)
		if err != nil {
			err := fmt.Errorf("unable to configure watchtower: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		tower, err = watchtower.New(wtConfig)
		if err != nil {
			err := fmt.Errorf("unable to create watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
	}

	// Initialize the ChainedAcceptor.
	chainedAcceptor := chanacceptor.NewChainedAcceptor()

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		Cfg, Cfg.Listeners, LocalChanDB, RemoteChanDB, towerClientDB,
		activeChainControl, &idKeyDesc, walletInitParams.ChansToRestore,
		chainedAcceptor, torController,UserId,
	)
	if err != nil {
		err := fmt.Errorf("unable to create server: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Set up an autopilot manager from the current config. This will be
	// used to manage the underlying autopilot agent, starting and stopping
	// it at will.
	atplCfg, err := initAutoPilot(server, Cfg.Autopilot, mainChain, Cfg.ActiveNetParams)
	if err != nil {
		err := fmt.Errorf("unable to initialize autopilot: %v", err)
		ltndLog.Error(err)
		return err
	}

	atplManager, err := autopilot.NewManager(atplCfg)
	if err != nil {
		err := fmt.Errorf("unable to create autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := atplManager.Start(); err != nil {
		err := fmt.Errorf("unable to start autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer atplManager.Stop()

	// rpcListeners is a closure we'll hand to the rpc server, that will be
	// called when it needs listeners for its GPRC server.
	rpcListeners := func() ([]*ListenerWithSignal, func(), error) {
		// If we have chosen to start with a dedicated listener for the
		// rpc server, we return it directly.
		if lisCfg.RPCListener != nil {
			return []*ListenerWithSignal{lisCfg.RPCListener},
				func() {}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners("lightningaction")
	}
	if fileExists(Cfg.graphDir + "/" + lncfg.DefaultConfigFilename) {

		}
		// restproxy des modified linked 2nd rpc port and 1nd rest port for lightning service
		restProxyDest = Cfg.RPCListeners[0].String() // in case when custom config is there otherwise wrong listen port
		switch {
		case strings.Contains(restProxyDest, "0.0.0.0"):
			restProxyDest = strings.Replace(
				restProxyDest, "0.0.0.0", "127.0.0.1", 1,
			)

		case strings.Contains(restProxyDest, "[::]"):
			restProxyDest = strings.Replace(
				restProxyDest, "[::]", "[::1]", 1,
			)
		}
		//ltndLog.Infof("config file path" + Cfg.ConfigFile)
	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer, err := newRPCServer(
		Cfg, server, macaroonService, Cfg.SubRPCServers, serverOpts,
		restDialOpts, restProxyDest, atplManager, server.invoices,
		tower, tlsCfg, rpcListeners, chainedAcceptor,UserId,Loader,
	)
		//code edit storing instances of rpcserve in slice
		RpcserverInstances = append(RpcserverInstances, rpcServer)
	if err != nil {
		err := fmt.Errorf("unable to create RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := rpcServer.Start(); err != nil {
		err := fmt.Errorf("unable to start RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer rpcServer.Stop()

	// If we're not in regtest or simnet mode, We'll wait until we're fully
	// synced to continue the start up of the remainder of the daemon. This
	// ensures that we don't accept any possibly invalid state transitions, or
	// accept channels with spent funds.
	if !(Cfg.Bitcoin.RegTest || Cfg.Bitcoin.SimNet ||
		Cfg.Litecoin.RegTest || Cfg.Litecoin.SimNet) {

		_, bestHeight, err := activeChainControl.chainIO.GetBestBlock()
		if err != nil {
			err := fmt.Errorf("unable to determine chain tip: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		ltndLog.Infof("Waiting for chain backend to finish sync, "+
			"start_height=%v", bestHeight)

		for {
			if !signal.Alive() {
				return nil
			}

			synced, _, err := activeChainControl.wallet.IsSynced()
			if err != nil {
				err := fmt.Errorf("unable to determine if "+
					"wallet is synced: %v", err)
				ltndLog.Error(err)
				return err
			}

			if synced {
				break
			}

			time.Sleep(time.Second * 1)
		}

		_, bestHeight, err = activeChainControl.chainIO.GetBestBlock()
		if err != nil {
			err := fmt.Errorf("unable to determine chain tip: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		ltndLog.Infof("Chain backend is fully synced (end_height=%v)!",
			bestHeight)
	}

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	if err := server.Start(); err != nil {
		err := fmt.Errorf("unable to start server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer server.Stop()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll start the autopilot agent immediately. It will be
	// stopped together with the autopilot service.
	if Cfg.Autopilot.Active {
		if err := atplManager.StartAgent(); err != nil {
			err := fmt.Errorf("unable to start autopilot agent: %v",
				err)
			ltndLog.Error(err)
			return err
		}
	}

	if Cfg.Watchtower.Active {
		if err := tower.Start(); err != nil {
			err := fmt.Errorf("unable to start watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer tower.Stop()
	}
      } //for loop ends

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-shutdownChan
	return nil
}

// getTLSConfig returns a TLS configuration for the gRPC server and credentials
// and a proxy destination for the REST reverse proxy.
func getTLSConfig(Cfg *Config) (*tls.Config, *credentials.TransportCredentials,
	func(), error) {

	// Ensure we create TLS key and certificate if they don't exist.
	if !fileExists(Cfg.TLSCertPath) && !fileExists(Cfg.TLSKeyPath) {
		rpcsLog.Infof("Generating TLS certificates...")
		err := cert.GenCertPair(
			"lnd autogenerated cert", Cfg.TLSCertPath,
			Cfg.TLSKeyPath, Cfg.TLSExtraIPs, Cfg.TLSExtraDomains,
			Cfg.TLSDisableAutofill, cert.DefaultAutogenValidity,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		rpcsLog.Infof("Done generating TLS certificates")
	}

	certData, parsedCert, err := cert.LoadCert(
		Cfg.TLSCertPath, Cfg.TLSKeyPath,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	// We check whether the certifcate we have on disk match the IPs and
	// domains specified by the config. If the extra IPs or domains have
	// changed from when the certificate was created, we will refresh the
	// certificate if auto refresh is active.
	refresh := false
	if Cfg.TLSAutoRefresh {
		refresh, err = cert.IsOutdated(
			parsedCert, Cfg.TLSExtraIPs,
			Cfg.TLSExtraDomains, Cfg.TLSDisableAutofill,
		)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	// If the certificate expired or it was outdated, delete it and the TLS
	// key and generate a new pair.
	if time.Now().After(parsedCert.NotAfter) || refresh {
		ltndLog.Info("TLS certificate is expired or outdated, " +
			"generating a new one")

		err := os.Remove(Cfg.TLSCertPath)
		if err != nil {
			return nil, nil, nil, err
		}

		err = os.Remove(Cfg.TLSKeyPath)
		if err != nil {
			return nil, nil, nil, err
		}

		rpcsLog.Infof("Renewing TLS certificates...")
		err = cert.GenCertPair(
			"lnd autogenerated cert", Cfg.TLSCertPath,
			Cfg.TLSKeyPath, Cfg.TLSExtraIPs, Cfg.TLSExtraDomains,
			Cfg.TLSDisableAutofill, cert.DefaultAutogenValidity,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		rpcsLog.Infof("Done renewing TLS certificates")

		// Reload the certificate data.
		certData, _, err = cert.LoadCert(
			Cfg.TLSCertPath, Cfg.TLSKeyPath,
		)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	tlsCfg := cert.TLSConfFromCert(certData)

	restCreds, err := credentials.NewClientTLSFromFile(Cfg.TLSCertPath, "")
	if err != nil {
		return nil, nil, nil, err
	}

	
	// If Let's Encrypt is enabled, instantiate autocert to request/renew
	// the certificates.
	cleanUp := func() {}
	if Cfg.LetsEncryptDomain != "" {
		ltndLog.Infof("Using Let's Encrypt certificate for domain %v",
			Cfg.LetsEncryptDomain)

		manager := autocert.Manager{
			Cache:      autocert.DirCache(Cfg.LetsEncryptDir),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(Cfg.LetsEncryptDomain),
		}

		srv := &http.Server{
			Addr:    Cfg.LetsEncryptListen,
			Handler: manager.HTTPHandler(nil),
		}
		shutdownCompleted := make(chan struct{})
		cleanUp = func() {
			err := srv.Shutdown(context.Background())
			if err != nil {
				ltndLog.Errorf("Autocert listener shutdown "+
					" error: %v", err)

				return
			}
			<-shutdownCompleted
			ltndLog.Infof("Autocert challenge listener stopped")
		}

		go func() {
			ltndLog.Infof("Autocert challenge listener started "+
				"at %v", Cfg.LetsEncryptListen)

			err := srv.ListenAndServe()
			if err != http.ErrServerClosed {
				ltndLog.Errorf("autocert http: %v", err)
			}
			close(shutdownCompleted)
		}()

		getCertificate := func(h *tls.ClientHelloInfo) (
			*tls.Certificate, error) {

			lecert, err := manager.GetCertificate(h)
			if err != nil {
				ltndLog.Errorf("GetCertificate: %v", err)
				return &certData, nil
			}

			return lecert, err
		}

		// The self-signed tls.cert remains available as fallback.
		tlsCfg.GetCertificate = getCertificate
	}

	return tlsCfg, &restCreds, cleanUp, nil
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/btcsuite/btcd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// genMacaroons generates three macaroon files; one admin-level, one for
// invoice access and one read-only. These can also be used to generate more
// granular macaroons.
func genMacaroons(ctx context.Context, svc *macaroons.Service,
	admFile, roFile, invoiceFile string) error {

	// First, we'll generate a macaroon that only allows the caller to
	// access invoice related calls. This is useful for merchants and other
	// services to allow an isolated instance that can only query and
	// modify invoices.
	invoiceMac, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, invoicePermissions...,
	)
	if err != nil {
		return err
	}
	invoiceMacBytes, err := invoiceMac.M().MarshalBinary()
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(invoiceFile, invoiceMacBytes, 0644)
	if err != nil {
		os.Remove(invoiceFile)
		return err
	}

	// Generate the read-only macaroon and write it to a file.
	roMacaroon, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, readPermissions...,
	)
	if err != nil {
		return err
	}
	roBytes, err := roMacaroon.M().MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(roFile, roBytes, 0644); err != nil {
		os.Remove(admFile)
		return err
	}

	// Generate the admin macaroon and write it to a file.
	adminPermissions := append(readPermissions, writePermissions...)
	admMacaroon, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, adminPermissions...,
	)
	if err != nil {
		return err
	}
	admBytes, err := admMacaroon.M().MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(admFile, admBytes, 0600); err != nil {
		return err
	}

	return nil
}

// WalletUnlockParams holds the variables used to parameterize the unlocking of
// lnd's wallet after it has already been created.
type WalletUnlockParams struct {
	// Password is the public and private wallet passphrase.
	Password []byte

	// Birthday specifies the approximate time that this wallet was created.
	// This is used to bound any rescans on startup.
	Birthday time.Time

	// RecoveryWindow specifies the address lookahead when entering recovery
	// mode. A recovery will be attempted if this value is non-zero.
	RecoveryWindow uint32

	// Wallet is the loaded and unlocked Wallet. This is returned
	// from the unlocker service to avoid it being unlocked twice (once in
	// the unlocker service to check if the password is correct and again
	// later when lnd actually uses it). Because unlocking involves scrypt
	// which is resource intensive, we want to avoid doing it twice.
	Wallet *wallet.Wallet

	// ChansToRestore a set of static channel backups that should be
	// restored before the main server instance starts up.
	ChansToRestore walletunlocker.ChannelsToRecover

	// UnloadWallet is a function for unloading the wallet, which should
	// be called on shutdown.
	UnloadWallet func() error
}

// waitForWalletPassword will spin up gRPC and REST endpoints for the
// WalletUnlocker server, and block until a password is provided by
// the user to this RPC server.
func waitForWalletPassword(Confg *Config, restEndpoints []net.Addr,
	serverOpts []grpc.ServerOption, restDialOpts []grpc.DialOption,
	restProxyDest string, tlsConf *tls.Config,
	getListeners rpcListeners) (*WalletUnlockParams, error) {

	// Start a gRPC server listening for HTTP/2 connections, solely used
	// for getting the encryption password from the client.
	listeners, cleanup, err := getListeners()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Set up a new PasswordService, which will listen for passwords
	// provided over RPC.
	grpcServer := grpc.NewServer(serverOpts...)
	defer grpcServer.GracefulStop()

	chainConfig := Confg.Bitcoin
	if Confg.registeredChains.PrimaryChain() == litecoinChain {
		chainConfig = Confg.Litecoin
	}

	// The macaroon files are passed to the wallet unlocker since they are
	// also encrypted with the wallet's password. These files will be
	// deleted within it and recreated when successfully changing the
	// wallet's password.
	macaroonFiles := []string{
		filepath.Join(Confg.graphDir, macaroons.DBFilename),
		Confg.AdminMacPath, Confg.ReadMacPath, Confg.InvoiceMacPath,
	}
	pwService := walletunlocker.New(
		chainConfig.ChainDir, Confg.ActiveNetParams.Params, !Confg.SyncFreelist,
		macaroonFiles,
	)
	lnrpc.RegisterWalletUnlockerServer(grpcServer, pwService)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, lis := range listeners {
		wg.Add(1)
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("password RPC server listening on %s",
				lis.Addr())

			// Close the ready chan to indicate we are listening.
			//close(lis.Ready)

			wg.Done()
			grpcServer.Serve(lis)
		}(lis)
	}

	// Start a REST proxy for our gRPC server above.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := proxy.NewServeMux()

	err = lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: allowCORS(mux, Confg.RestCORS)}

	for i, restEndpoint := range restEndpoints {
		//try changing the logic unnecessary calls evertime rest is set for the same rest port i.e 1st one
		if i == 0 {
			lis, err := lncfg.TLSListenOnAddress(restEndpoint, tlsConf)
			if err != nil {
				ltndLog.Errorf(
					"password gRPC proxy unable to listen on %s",
					restEndpoint,
				)
				continue
				//return nil, err //
			}
			defer lis.Close()

			wg.Add(1)
			go func() {
				rpcsLog.Infof(
					"password gRPC proxy started at %s",
					lis.Addr(),
				)
				wg.Done()
				srv.Serve(lis)
			}()
			break
		}
	}

	// Wait for gRPC and REST servers to be up running.
	wg.Wait()

	// Wait for user to provide the password.
	ltndLog.Infof("Waiting for wallet encryption password. Use `lncli " +
		"create` to create a wallet, `lncli unlock` to unlock an " +
		"existing wallet, or `lncli changepassword` to change the " +
		"password of an existing wallet and unlock it.")

	// We currently don't distinguish between getting a password to be used
	// for creation or unlocking, as a new wallet db will be created if
	// none exists when creating the chain control.
	select {

	// The wallet is being created for the first time, we'll check to see
	// if the user provided any entropy for seed creation. If so, then
	// we'll create the wallet early to load the seed.
	case initMsg := <-pwService.InitMsgs:
		password := initMsg.Passphrase
		cipherSeed := initMsg.WalletSeed
		recoveryWindow := initMsg.RecoveryWindow

		// Before we proceed, we'll check the internal version of the
		// seed. If it's greater than the current key derivation
		// version, then we'll return an error as we don't understand
		// this.
		if cipherSeed.InternalVersion != keychain.KeyDerivationVersion {
			return nil, fmt.Errorf("invalid internal seed version "+
				"%v, current version is %v",
				cipherSeed.InternalVersion,
				keychain.KeyDerivationVersion)
		}

			Confg.graphDir = filepath.Join("test_data_PrvW",
			defaultGraphSubDirname,
			normalizeNetwork(Confg.ActiveNetParams.Name), initMsg.UniqueId)

		// added userid for multiple server instance
		UserId = initMsg.UniqueId

		//custom configuration code edit for each node
		Cfg = Controller_Config
                if fileExists(Confg.graphDir + "/" + lncfg.DefaultConfigFilename) {
		 loadedConfig, err := LoadConfig(UserId) //returns a new instance of config accordingly
		 if err != nil {
		   return nil,err
		 }
		 Cfg = loadedConfig
		}		
		//ltndLog.Infof("config file path" + Cfg.ConfigFile)
		//code modify by -----start--------
		//netDir := btcwallet.NetworkDir(
		//	chainConfig.ChainDir, Confg.ActiveNetParams.Params,
		//)
		Cfg.graphDir = filepath.Join("test_data_PrvW",
			defaultGraphSubDirname,
			normalizeNetwork(Confg.ActiveNetParams.Name), initMsg.UniqueId)
		netDir := Cfg.graphDir
		//code modify by -----end--------
		loader := wallet.NewLoader(
			Cfg.ActiveNetParams.Params, netDir, !Cfg.SyncFreelist,
			recoveryWindow,
		)

		// With the seed, we can now use the wallet loader to create
		// the wallet, then pass it back to avoid unlocking it again.
		birthday := cipherSeed.BirthdayTime()
		newWallet, err := loader.CreateNewWallet(
			password, password, cipherSeed.Entropy[:], birthday,
		)
		if err != nil {
			// Don't leave the file open in case the new wallet
			// could not be created for whatever reason.
			if err := loader.UnloadWallet(); err != nil {
				ltndLog.Errorf("Could not unload new "+
					"wallet: %v", err)
			}
			return nil, err
		}
/////----channel.db -----
		LocalChanDB, RemoteChanDB, _, err = initializeDatabases(ctx, Cfg,initMsg.UniqueId)
	switch {
	case err == channeldb.ErrDryRunMigrationOK:
		ltndLog.Infof("%v, exiting", err)
		return nil,nil
	case err != nil:
		return nil ,nil
	}
		
		ltndLog.Infof("lnd.go after opening channeldb.open channeled opened success")
		return &WalletUnlockParams{
			Password:       password,
			Birthday:       birthday,
			RecoveryWindow: recoveryWindow,
			Wallet:         newWallet,
			ChansToRestore: initMsg.ChanBackups,
			UnloadWallet:   loader.UnloadWallet,
		}, nil



	// The wallet has already been created in the past, and is simply being
	// unlocked. So we'll just return these passphrases.
	case unlockMsg := <-pwService.UnlockMsgs:

		Confg.graphDir = filepath.Join("test_data_PrvW",
			defaultGraphSubDirname,
			normalizeNetwork(Confg.ActiveNetParams.Name), unlockMsg.UniqueId)
		// added userid for multiple server instance
		UserId = unlockMsg.UniqueId
		//custom configuration code edit for each node
		Cfg = Controller_Config
		ltndLog.Infof("lnd.go before loading configuration")
                if fileExists(Confg.graphDir + "/" + lncfg.DefaultConfigFilename) {
		 loadedConfig, err := LoadConfig(UserId) //returns a new instance of config accordingly
		 if err != nil {
		   return nil,err
		 }
		 Cfg = loadedConfig
		}
		ltndLog.Infof("lnd.go after loading configuration")
		ltndLog.Infof("config file path" + Cfg.ConfigFile)
		
         	Cfg.graphDir = filepath.Join("test_data_PrvW",
			defaultGraphSubDirname,
			normalizeNetwork(Cfg.ActiveNetParams.Name), unlockMsg.UniqueId)
		/////----channel.db -----
		LocalChanDB, RemoteChanDB, _, err = initializeDatabases(ctx, Cfg,unlockMsg.UniqueId)
	switch {
	case err == channeldb.ErrDryRunMigrationOK:
		ltndLog.Infof("%v, exiting", err)
		return nil,nil
	case err != nil:
		return nil,nil
	}

		ltndLog.Infof("lnd.go after opening channeldb.open channeled opened success")
			
		Loader = unlockMsg.Loader	
		return &WalletUnlockParams{
			Password:       unlockMsg.Passphrase,
			RecoveryWindow: unlockMsg.RecoveryWindow,
			Wallet:         unlockMsg.Wallet,
			ChansToRestore: unlockMsg.ChanBackups,
			UnloadWallet:   unlockMsg.UnloadWallet,
		}, nil

	case <-signal.ShutdownChannel():
		return nil, fmt.Errorf("shutting down")
	}
}

// initializeDatabases extracts the current databases that we'll use for normal
// operation in the daemon. Two databases are returned: one remote and one
// local. However, only if the replicated database is active will the remote
// database point to a unique database. Otherwise, the local and remote DB will
// both point to the same local database. A function closure that closes all
// opened databases is also returned.
func initializeDatabases(ctx context.Context,
	Cfg *Config,UserId string) (*channeldb.DB, *channeldb.DB, func(), error) {

	ltndLog.Infof("Opening the main database, this might take a few " +
		"minutes...")

	if Cfg.DB.Backend == lncfg.BoltBackend {
		ltndLog.Infof("Opening bbolt database, sync_freelist=%v",
			Cfg.DB.Bolt.SyncFreelist)
	}

	startOpenTime := time.Now()

	databaseBackends, err := Cfg.DB.GetBackends(
		ctx, Cfg.localDatabaseDir(UserId), Cfg.networkName(),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to obtain database "+
			"backends: %v", err)
	}

	// If the remoteDB is nil, then we'll just open a local DB as normal,
	// having the remote and local pointer be the exact same instance.
	var (
		localChanDB, remoteChanDB *channeldb.DB
		closeFuncs                []func()
	)
	if databaseBackends.RemoteDB == nil {
		// Open the channeldb, which is dedicated to storing channel,
		// and network related metadata.
		localChanDB, err = channeldb.CreateWithBackend(
			databaseBackends.LocalDB,
			channeldb.OptionSetRejectCacheSize(Cfg.Caches.RejectCacheSize),
			channeldb.OptionSetChannelCacheSize(Cfg.Caches.ChannelCacheSize),
			channeldb.OptionDryRunMigration(Cfg.DryRunMigration),
		)
		switch {
		case err == channeldb.ErrDryRunMigrationOK:
			return nil, nil, nil, err

		case err != nil:
			err := fmt.Errorf("unable to open local channeldb: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		closeFuncs = append(closeFuncs, func() {
			localChanDB.Close()
		})

		remoteChanDB = localChanDB
	} else {
		ltndLog.Infof("Database replication is available! Creating " +
			"local and remote channeldb instances")

		// Otherwise, we'll open two instances, one for the state we
		// only need locally, and the other for things we want to
		// ensure are replicated.
		localChanDB, err = channeldb.CreateWithBackend(
			databaseBackends.LocalDB,
			channeldb.OptionSetRejectCacheSize(Cfg.Caches.RejectCacheSize),
			channeldb.OptionSetChannelCacheSize(Cfg.Caches.ChannelCacheSize),
			channeldb.OptionDryRunMigration(Cfg.DryRunMigration),
		)
		switch {
		// As we want to allow both versions to get thru the dry run
		// migration, we'll only exit the second time here once the
		// remote instance has had a time to migrate as well.
		case err == channeldb.ErrDryRunMigrationOK:
			ltndLog.Infof("Local DB dry run migration successful")

		case err != nil:
			err := fmt.Errorf("unable to open local channeldb: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		closeFuncs = append(closeFuncs, func() {
			localChanDB.Close()
		})

		ltndLog.Infof("Opening replicated database instance...")

		remoteChanDB, err = channeldb.CreateWithBackend(
			databaseBackends.RemoteDB,
			channeldb.OptionDryRunMigration(Cfg.DryRunMigration),
		)
		switch {
		case err == channeldb.ErrDryRunMigrationOK:
			return nil, nil, nil, err

		case err != nil:
			localChanDB.Close()

			err := fmt.Errorf("unable to open remote channeldb: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		closeFuncs = append(closeFuncs, func() {
			remoteChanDB.Close()
		})
	}

	openTime := time.Since(startOpenTime)
	ltndLog.Infof("Database now open (time_to_open=%v)!", openTime)

	cleanUp := func() {
		for _, closeFunc := range closeFuncs {
			closeFunc()
		}
	}

	return localChanDB, remoteChanDB, cleanUp, nil
}
