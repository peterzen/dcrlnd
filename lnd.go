// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2019 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package dcrlnd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	// Blank import to set up profiling HTTP handlers.
	_ "net/http/pprof"

	"gopkg.in/macaroon-bakery.v2/bakery"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	walletloader "github.com/decred/dcrlnd/lnwallet/dcrwallet/loader"
	"github.com/decred/dcrwallet/wallet/v3"
	"github.com/decred/dcrwallet/wallet/v3/txrules"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"

	"github.com/decred/dcrlnd/autopilot"
	"github.com/decred/dcrlnd/build"
	"github.com/decred/dcrlnd/chanacceptor"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/keychain"
	"github.com/decred/dcrlnd/lncfg"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/dcrwallet"
	"github.com/decred/dcrlnd/macaroons"
	"github.com/decred/dcrlnd/signal"
	"github.com/decred/dcrlnd/walletunlocker"
	"github.com/decred/dcrlnd/watchtower"
	"github.com/decred/dcrlnd/watchtower/wtdb"
)

const (
	// Make certificate valid for 14 months.
	autogenCertValidity = 14 /*months*/ * 30 /*days*/ * 24 * time.Hour
)

var (
	cfg              *config
	registeredChains = newChainRegistry()

	// networkDir is the path to the directory of the currently active
	// network. This path will hold the files related to each different
	// network.
	networkDir string

	// End of ASN.1 time.
	endOfTime = time.Date(2049, 12, 31, 23, 59, 59, 0, time.UTC)

	// Max serial number.
	serialNumberLimit = new(big.Int).Lsh(big.NewInt(1), 128)

	/*
	 * These cipher suites fit the following criteria:
	 * - Don't use outdated algorithms like SHA-1 and 3DES
	 * - Don't use ECB mode or other insecure symmetric methods
	 * - Included in the TLS v1.2 suite
	 * - Are available in the Go 1.7.6 standard library (more are
	 *   available in 1.8.3 and will be added after lnd no longer
	 *   supports 1.7, including suites that support CBC mode)
	**/
	tlsCipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	}
)

// ListenerCfg is a wrapper around custom listeners that can be passed to lnd
// when calling its main method.
type ListenerCfg struct {
	// WalletUnlocker can be set to the listener to use for the wallet
	// unlocker. If nil a regular network listener will be created.
	WalletUnlocker net.Listener

	// RPCListener can be set to the listener to use for the RPC server. If
	// nil a regular network listener will be created.
	RPCListener net.Listener
}

// rpcListeners is a function type used for closures that fetches a set of RPC
// listeners for the current configuration, and the GRPC server options to use
// with these listeners. If no custom listeners are present, this should return
// normal listeners from the RPC endpoints defined in the config, and server
// options specifying TLS.
type rpcListeners func() ([]net.Listener, func(), []grpc.ServerOption, error)

// Main is the true entry point for lnd. This function is required since defers
// created in the top-level scope of a main method aren't executed if os.Exit()
// is called.
func Main(lisCfg ListenerCfg) error {
	// Load the configuration, and parse any command line options. This
	// function will also set up logging properly.
	loadedConfig, err := loadConfig()
	if err != nil {
		return err
	}
	cfg = loadedConfig
	defer func() {
		if logRotator != nil {
			ltndLog.Info("Shutdown complete")
			logRotator.Close()
		}
	}()

	// Show version at startup.
	ltndLog.Infof("Version: %s, build=%s, logging=%s",
		build.Version(), build.Deployment, build.LoggingType)

	var network string
	switch {
	case cfg.Decred.TestNet3:
		network = "testnet"

	case cfg.Decred.MainNet:
		network = "mainnet"

	case cfg.Decred.SimNet:
		network = "simnet"

	case cfg.Decred.RegTest:
		network = "regtest"
	}

	ltndLog.Infof("Active chain: %v (network=%v)",
		strings.Title(registeredChains.PrimaryChain().String()),
		network,
	)

	// Enable http profiling server if requested.
	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Write cpu profile if requested.
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			err := fmt.Errorf("Unable to create CPU profile: %v",
				err)
			ltndLog.Error(err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	// Create the network-segmented directory for the channel database.
	graphDir := filepath.Join(cfg.DataDir,
		defaultGraphSubDirname,
		normalizeNetwork(activeNetParams.Name))

	// Open the channeldb, which is dedicated to storing channel, and
	// network related metadata.
	chanDB, err := channeldb.Open(
		graphDir,
		channeldb.OptionSetRejectCacheSize(cfg.Caches.RejectCacheSize),
		channeldb.OptionSetChannelCacheSize(cfg.Caches.ChannelCacheSize),
		channeldb.OptionSetSyncFreelist(cfg.SyncFreelist),
	)
	if err != nil {
		err := fmt.Errorf("Unable to open channeldb: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer chanDB.Close()

	// Only process macaroons if --no-macaroons isn't set.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tlsCfg, restCreds, restProxyDest, err := getTLSConfig(
		cfg.TLSCertPath, cfg.TLSKeyPath, cfg.TLSExtraIPs,
		cfg.TLSExtraDomains, cfg.RPCListeners,
	)
	if err != nil {
		err := fmt.Errorf("Unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
	}

	serverCreds := credentials.NewTLS(tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// For our REST dial options, we'll still use TLS, but also increase
	// the max message size that we'll decode to allow clients to hit
	// endpoints which return more data such as the DescribeGraph call.
	restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(*restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 50),
		),
	}

	var (
		walletInitParams WalletUnlockParams
		privateWalletPw  = lnwallet.DefaultPrivatePassphrase
		publicWalletPw   = lnwallet.DefaultPublicPassphrase
	)

	// If the user didn't request a seed, then we'll manually assume a
	// wallet birthday of now, as otherwise the seed would've specified
	// this information.
	walletInitParams.Birthday = time.Now()

	isRemoteWallet := cfg.Dcrwallet.GRPCHost != "" && cfg.Dcrwallet.CertPath != ""

	// getListeners is a closure that creates listeners from the
	// RPCListeners defined in the config. It also returns a cleanup
	// closure and the server options to use for the GRPC server.
	getListeners := func() ([]net.Listener, func(), []grpc.ServerOption,
		error) {

		var grpcListeners []net.Listener
		for _, grpcEndpoint := range cfg.RPCListeners {
			// Start a gRPC server listening for HTTP/2
			// connections.
			lis, err := lncfg.ListenOnAddress(grpcEndpoint)
			if err != nil {
				ltndLog.Errorf("unable to listen on %s",
					grpcEndpoint)
				return nil, nil, nil, err
			}
			grpcListeners = append(grpcListeners, lis)
		}

		cleanup := func() {
			for _, lis := range grpcListeners {
				lis.Close()
			}
		}
		return grpcListeners, cleanup, serverOpts, nil
	}

	// walletUnlockerListeners is a closure we'll hand to the wallet
	// unlocker, that will be called when it needs listeners for its GPRC
	// server.
	walletUnlockerListeners := func() ([]net.Listener, func(),
		[]grpc.ServerOption, error) {

		// If we have chosen to start with a dedicated listener for the
		// wallet unlocker, we return it directly, and empty server
		// options to deactivate TLS.
		// TODO(halseth): any point in adding TLS support for custom
		// listeners?
		if lisCfg.WalletUnlocker != nil {
			return []net.Listener{lisCfg.WalletUnlocker}, func() {},
				[]grpc.ServerOption{}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners()
	}

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noseedbackup flag, we use the default password
	// for wallet encryption.
	if !cfg.NoSeedBackup || isRemoteWallet {
		params, err := waitForWalletPassword(
			cfg.RESTListeners, restDialOpts, restProxyDest, tlsCfg,
			walletUnlockerListeners,
		)
		if err != nil {
			err := fmt.Errorf("Unable to set up wallet password "+
				"listeners: %v", err)
			ltndLog.Error(err)
			return err
		}

		walletInitParams = *params
		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password

		if walletInitParams.RecoveryWindow > 0 {
			ltndLog.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				walletInitParams.RecoveryWindow)
		}
	}

	var macaroonService *macaroons.Service
	if !cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		macaroonService, err = macaroons.NewService(
			networkDir, macaroons.IPLockChecker,
		)
		if err != nil {
			err := fmt.Errorf("Unable to set up macaroon "+
				"authentication: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer macaroonService.Close()

		// Try to unlock the macaroon store with the private password.
		err = macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil {
			err := fmt.Errorf("Unable to unlock macaroons: %v", err)
			ltndLog.Error(err)
			return err
		}

		// Create macaroon files for dcrlncli to use if they don't exist.
		if !fileExists(cfg.AdminMacPath) && !fileExists(cfg.ReadMacPath) &&
			!fileExists(cfg.InvoiceMacPath) {

			err = genMacaroons(
				ctx, macaroonService, cfg.AdminMacPath,
				cfg.ReadMacPath, cfg.InvoiceMacPath,
			)
			if err != nil {
				err := fmt.Errorf("Unable to create macaroons "+
					"%v", err)
				ltndLog.Error(err)
				return err
			}
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	activeChainControl, err := newChainControlFromConfig(
		cfg, chanDB, privateWalletPw, publicWalletPw,
		walletInitParams.Birthday, walletInitParams.RecoveryWindow,
		walletInitParams.Wallet, walletInitParams.Loader,
		walletInitParams.Conn, cfg.Dcrwallet.AccountNumber,
	)
	if err != nil {
		err := fmt.Errorf("Unable to create chain control: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Wait until we're fully synced to continue the start up of the remainder
	// of the daemon. This ensures that we don't accept any possibly invalid
	// state transitions, or accept channels with spent funds.
	//
	// This is also required on decred due to various things dcrwallet does at
	// startup (mainly, slip0044 upgrade, account and address discovery) which
	// may deadlock if we try to start using it to (e.g.) derive the node's id
	// private key before the wallet is fully synced for the first time.
	_, bestHeight, err := activeChainControl.chainIO.GetBestBlock()
	if err != nil {
		return err
	}

	ltndLog.Infof("Waiting for chain backend to finish sync, "+
		"start_height=%v", bestHeight)

	select {
	case <-signal.ShutdownChannel():
		return nil
	case <-activeChainControl.wallet.InitialSyncChannel():
	}

	_, bestHeight, err = activeChainControl.chainIO.GetBestBlock()
	if err != nil {
		return err
	}

	ltndLog.Infof("Chain backend is fully synced (end_height=%v)!",
		bestHeight)

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := registeredChains.PrimaryChain()
	registeredChains.RegisterChain(primaryChain, activeChainControl)

	// TODO(roasbeef): add rotation
	idPrivKey, err := activeChainControl.wallet.DerivePrivKey(keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	})
	if err != nil {
		err := fmt.Errorf("Unable to derive node private key: %v", err)
		ltndLog.Error(err)
		return err
	}
	idPrivKey.Curve = secp256k1.S256()
	idPubKey := idPrivKey.PubKey()
	srvrLog.Infof("Derived node public key %x", idPubKey.Serialize())

	if cfg.Tor.Active {
		srvrLog.Infof("Proxying all network traffic via Tor "+
			"(stream_isolation=%v)! NOTE: Ensure the backend node "+
			"is proxying over Tor as well", cfg.Tor.StreamIsolation)
	}

	// If the watchtower client should be active, open the client database.
	// This is done here so that Close always executes when lndMain returns.
	var towerClientDB *wtdb.ClientDB
	if cfg.WtClient.Active {
		var err error
		towerClientDB, err = wtdb.OpenClientDB(graphDir)
		if err != nil {
			err := fmt.Errorf("Unable to open watchtower client "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerClientDB.Close()
	}

	var tower *watchtower.Standalone
	if cfg.Watchtower.Active {
		// Segment the watchtower directory by chain and network.
		towerDBDir := filepath.Join(
			cfg.Watchtower.TowerDir,
			registeredChains.PrimaryChain().String(),
			normalizeNetwork(activeNetParams.Name),
		)

		towerDB, err := wtdb.OpenTowerDB(towerDBDir)
		if err != nil {
			err := fmt.Errorf("Unable to open watchtower "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerDB.Close()

		towerPrivKey, err := activeChainControl.wallet.DerivePrivKey(
			keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamilyTowerID,
					Index:  0,
				},
			},
		)
		if err != nil {
			err := fmt.Errorf("Unable to derive watchtower "+
				"private key: %v", err)
			ltndLog.Error(err)
			return err
		}

		wtConfig, err := cfg.Watchtower.Apply(&watchtower.Config{
			NetParams:      activeNetParams.Params,
			BlockFetcher:   activeChainControl.chainIO,
			DB:             towerDB,
			EpochRegistrar: activeChainControl.chainNotifier,
			Net:            cfg.net,
			NewAddress: func() (dcrutil.Address, error) {
				return activeChainControl.wallet.NewAddress(
					lnwallet.WitnessPubKey, false,
				)
			},
			NodePrivKey: towerPrivKey,
			PublishTx:   activeChainControl.wallet.PublishTransaction,
			ChainHash:   activeNetParams.GenesisHash,
		}, lncfg.NormalizeAddresses)
		if err != nil {
			err := fmt.Errorf("Unable to configure watchtower: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		tower, err = watchtower.New(wtConfig)
		if err != nil {
			err := fmt.Errorf("Unable to create watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
	}

	// Initialize the ChainedAcceptor.
	chainedAcceptor := chanacceptor.NewChainedAcceptor()

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		cfg.Listeners, chanDB, towerClientDB, activeChainControl,
		idPrivKey, walletInitParams.ChansToRestore, chainedAcceptor,
	)
	if err != nil {
		err := fmt.Errorf("Unable to create server: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Set up an autopilot manager from the current config. This will be
	// used to manage the underlying autopilot agent, starting and stopping
	// it at will.
	atplCfg, err := initAutoPilot(server, cfg.Autopilot)
	if err != nil {
		err := fmt.Errorf("Unable to initialize autopilot: %v", err)
		ltndLog.Error(err)
		return err
	}

	atplManager, err := autopilot.NewManager(atplCfg)
	if err != nil {
		err := fmt.Errorf("Unable to create autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := atplManager.Start(); err != nil {
		err := fmt.Errorf("Unable to start autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer atplManager.Stop()

	// rpcListeners is a closure we'll hand to the rpc server, that will be
	// called when it needs listeners for its GPRC server.
	rpcListeners := func() ([]net.Listener, func(), []grpc.ServerOption,
		error) {

		// If we have chosen to start with a dedicated listener for the
		// rpc server, we return it directly, and empty server options
		// to deactivate TLS.
		// TODO(halseth): any point in adding TLS support for custom
		// listeners?
		if lisCfg.RPCListener != nil {
			return []net.Listener{lisCfg.RPCListener}, func() {},
				[]grpc.ServerOption{}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners()
	}

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer, err := newRPCServer(
		server, macaroonService, cfg.SubRPCServers, restDialOpts,
		restProxyDest, atplManager, server.invoices, tower, tlsCfg,
		rpcListeners, chainedAcceptor,
	)
	if err != nil {
		err := fmt.Errorf("Unable to create RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := rpcServer.Start(); err != nil {
		err := fmt.Errorf("Unable to start RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer rpcServer.Stop()

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	if err := server.Start(); err != nil {
		err := fmt.Errorf("Unable to start server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer server.Stop()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll start the autopilot agent immediately. It will be
	// stopped together with the autopilot service.
	if cfg.Autopilot.Active {
		if err := atplManager.StartAgent(); err != nil {
			err := fmt.Errorf("Unable to start autopilot agent: %v",
				err)
			ltndLog.Error(err)
			return err
		}
	}

	if cfg.Watchtower.Active {
		if err := tower.Start(); err != nil {
			err := fmt.Errorf("Unable to start watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer tower.Stop()
	}

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-signal.ShutdownChannel()
	return nil
}

// getTLSConfig returns a TLS configuration for the gRPC server and credentials
// and a proxy destination for the REST reverse proxy.
func getTLSConfig(tlsCertPath string, tlsKeyPath string, tlsExtraIPs,
	tlsExtraDomains []string, rpcListeners []net.Addr) (*tls.Config,
	*credentials.TransportCredentials, string, error) {

	// Ensure we create TLS key and certificate if they don't exist
	if !fileExists(tlsCertPath) && !fileExists(tlsKeyPath) {
		err := genCertPair(
			tlsCertPath, tlsKeyPath, tlsExtraIPs, tlsExtraDomains,
		)
		if err != nil {
			return nil, nil, "", err
		}
	}

	certData, err := tls.LoadX509KeyPair(tlsCertPath, tlsKeyPath)
	if err != nil {
		return nil, nil, "", err
	}

	cert, err := x509.ParseCertificate(certData.Certificate[0])
	if err != nil {
		return nil, nil, "", err
	}

	// If the certificate expired, delete it and the TLS key and generate a
	// new pair
	if time.Now().After(cert.NotAfter) {
		ltndLog.Info("TLS certificate is expired, generating a new one")

		err := os.Remove(tlsCertPath)
		if err != nil {
			return nil, nil, "", err
		}

		err = os.Remove(tlsKeyPath)
		if err != nil {
			return nil, nil, "", err
		}

		err = genCertPair(
			tlsCertPath, tlsKeyPath, tlsExtraIPs, tlsExtraDomains,
		)
		if err != nil {
			return nil, nil, "", err
		}
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{certData},
		CipherSuites: tlsCipherSuites,
		MinVersion:   tls.VersionTLS12,
	}

	restCreds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		return nil, nil, "", err
	}

	restProxyDest := rpcListeners[0].String()
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

	return tlsCfg, &restCreds, restProxyDest, nil
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/decred/dcrd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// genCertPair generates a key/cert pair to the paths provided. The
// auto-generated certificates should *not* be used in production for public
// access as they're self-signed and don't necessarily contain all of the
// desired hostnames for the service. For production/public use, consider a
// real PKI.
//
// This function is adapted from https://github.com/decred/dcrd and
// https://github.com/decred/dcrd/dcrutil/v2
func genCertPair(certFile, keyFile string, tlsExtraIPs,
	tlsExtraDomains []string) error {

	rpcsLog.Infof("Generating TLS certificates...")

	org := "lnd autogenerated cert"
	now := time.Now()
	validUntil := now.Add(autogenCertValidity)

	// Check that the certificate validity isn't past the ASN.1 end of time.
	if validUntil.After(endOfTime) {
		validUntil = endOfTime
	}

	// Generate a serial number that's below the serialNumberLimit.
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %s", err)
	}

	// Collect the host's IP addresses, including loopback, in a slice.
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	// addIP appends an IP address only if it isn't already in the slice.
	addIP := func(ipAddr net.IP) {
		for _, ip := range ipAddresses {
			if net.IP.Equal(ip, ipAddr) {
				return
			}
		}
		ipAddresses = append(ipAddresses, ipAddr)
	}

	// Add all the interface IPs that aren't already in the slice.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		ipAddr, _, err := net.ParseCIDR(a.String())
		if err == nil {
			addIP(ipAddr)
		}
	}

	// Add extra IPs to the slice.
	for _, ip := range tlsExtraIPs {
		ipAddr := net.ParseIP(ip)
		if ipAddr != nil {
			addIP(ipAddr)
		}
	}

	// Collect the host's names into a slice.
	host, err := os.Hostname()
	if err != nil {
		rpcsLog.Errorf("Failed getting hostname, falling back to "+
			"localhost: %v", err)
		host = "localhost"
	}

	dnsNames := []string{host}
	if host != "localhost" {
		dnsNames = append(dnsNames, "localhost")
	}
	dnsNames = append(dnsNames, tlsExtraDomains...)

	// Also add fake hostnames for unix sockets, otherwise hostname
	// verification will fail in the client.
	dnsNames = append(dnsNames, "unix", "unixpacket")

	// Generate a private key for the certificate.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	// Construct the certificate template.
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{org},
			CommonName:   host,
		},
		NotBefore: now.Add(-time.Hour * 24),
		NotAfter:  validUntil,

		KeyUsage: x509.KeyUsageKeyEncipherment |
			x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true, // so can sign self.
		BasicConstraintsValid: true,

		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template,
		&template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %v", err)
	}

	certBuf := &bytes.Buffer{}
	err = pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE",
		Bytes: derBytes})
	if err != nil {
		return fmt.Errorf("failed to encode certificate: %v", err)
	}

	keybytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("unable to encode privkey: %v", err)
	}
	keyBuf := &bytes.Buffer{}
	err = pem.Encode(keyBuf, &pem.Block{Type: "EC PRIVATE KEY",
		Bytes: keybytes})
	if err != nil {
		return fmt.Errorf("failed to encode private key: %v", err)
	}

	// Write cert and key files.
	if err = ioutil.WriteFile(certFile, certBuf.Bytes(), 0644); err != nil {
		return err
	}
	if err = ioutil.WriteFile(keyFile, keyBuf.Bytes(), 0600); err != nil {
		os.Remove(certFile)
		return err
	}

	rpcsLog.Infof("Done generating TLS certificates")
	return nil
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
	invoiceMac, err := svc.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, invoicePermissions...,
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
	roMacaroon, err := svc.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, readPermissions...,
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
	admMacaroon, err := svc.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, adminPermissions...,
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

	// Loader is the wallet loader used to create or open the corresponding
	// wallet.
	Loader *walletloader.Loader

	// Conn is the connection to the remote wallet when that is used
	// instead of an embedded dcrwallet instance.
	Conn *grpc.ClientConn

	// ChansToRestore a set of static channel backups that should be
	// restored before the main server instance starts up.
	ChansToRestore walletunlocker.ChannelsToRecover
}

// waitForWalletPassword will spin up gRPC and REST endpoints for the
// WalletUnlocker server, and block until a password is provided by
// the user to this RPC server.
func waitForWalletPassword(restEndpoints []net.Addr,
	restDialOpts []grpc.DialOption, restProxyDest string,
	tlsConf *tls.Config, getListeners rpcListeners) (
	*WalletUnlockParams, error) {

	// Start a gRPC server listening for HTTP/2 connections, solely used
	// for getting the encryption password from the client.
	listeners, cleanup, serverOpts, err := getListeners()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Set up a new PasswordService, which will listen for passwords
	// provided over RPC.
	grpcServer := grpc.NewServer(serverOpts...)
	defer grpcServer.GracefulStop()

	chainConfig := cfg.Decred

	// The macaroon files are passed to the wallet unlocker since they are
	// also encrypted with the wallet's password. These files will be
	// deleted within it and recreated when successfully changing the
	// wallet's password.
	macaroonFiles := []string{
		filepath.Join(networkDir, macaroons.DBFilename),
		cfg.AdminMacPath, cfg.ReadMacPath, cfg.InvoiceMacPath,
	}
	pwService := walletunlocker.New(
		chainConfig.ChainDir, activeNetParams.Params, !cfg.SyncFreelist,
		macaroonFiles, cfg.Dcrwallet.GRPCHost, cfg.Dcrwallet.CertPath,
		cfg.Dcrwallet.AccountNumber,
	)
	lnrpc.RegisterWalletUnlockerServer(grpcServer, pwService)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, lis := range listeners {
		wg.Add(1)
		go func(lis net.Listener) {
			rpcsLog.Infof("password RPC server listening on %s",
				lis.Addr())
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

	srv := &http.Server{Handler: mux}

	for _, restEndpoint := range restEndpoints {
		lis, err := lncfg.TLSListenOnAddress(restEndpoint, tlsConf)
		if err != nil {
			ltndLog.Errorf(
				"password gRPC proxy unable to listen on %s",
				restEndpoint,
			)
			return nil, err
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
	}

	// Wait for gRPC and REST servers to be up running.
	wg.Wait()

	// Wait for user to provide the password.
	ltndLog.Infof("Waiting for wallet encryption password. Use `dcrlncli " +
		"create` to create a wallet, `dcrlncli unlock` to unlock an " +
		"existing wallet, or `dcrlncli changepassword` to change the " +
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

		netDir := dcrwallet.NetworkDir(
			chainConfig.ChainDir, activeNetParams.Params,
		)
		loader := walletloader.NewLoader(activeNetParams.Params, netDir,
			&walletloader.StakeOptions{}, wallet.DefaultGapLimit, false,
			txrules.DefaultRelayFeePerKb.ToCoin(), wallet.DefaultAccountGapLimit,
			false)

		// With the seed, we can now use the wallet loader to create
		// the wallet, then pass it back to avoid unlocking it again.
		birthday := cipherSeed.BirthdayTime()
		newWallet, err := loader.CreateNewWallet(
			context.TODO(), password, password, cipherSeed.Entropy[:],
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

		return &WalletUnlockParams{
			Password:       password,
			Birthday:       birthday,
			RecoveryWindow: recoveryWindow,
			Wallet:         newWallet,
			Loader:         loader,
			ChansToRestore: initMsg.ChanBackups,
		}, nil

	// The wallet has already been created in the past, and is simply being
	// unlocked. So we'll just return these passphrases.
	case unlockMsg := <-pwService.UnlockMsgs:
		return &WalletUnlockParams{
			Password:       unlockMsg.Passphrase,
			RecoveryWindow: unlockMsg.RecoveryWindow,
			Wallet:         unlockMsg.Wallet,
			Loader:         unlockMsg.Loader,
			ChansToRestore: unlockMsg.ChanBackups,
			Conn:           unlockMsg.Conn,
		}, nil

	case <-signal.ShutdownChannel():
		return nil, fmt.Errorf("shutting down")
	}
}
