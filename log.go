package dcrlnd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/decred/dcrd/connmgr"
	"github.com/decred/dcrlnd/autopilot"
	"github.com/decred/dcrlnd/build"
	"github.com/decred/dcrlnd/chainntnfs"
	"github.com/decred/dcrlnd/chanbackup"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/channelnotifier"
	"github.com/decred/dcrlnd/contractcourt"
	"github.com/decred/dcrlnd/discovery"
	"github.com/decred/dcrlnd/htlcswitch"
	"github.com/decred/dcrlnd/invoices"
	"github.com/decred/dcrlnd/keychain"
	"github.com/decred/dcrlnd/lnrpc/autopilotrpc"
	"github.com/decred/dcrlnd/lnrpc/chainrpc"
	"github.com/decred/dcrlnd/lnrpc/invoicesrpc"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"github.com/decred/dcrlnd/lnrpc/signrpc"
	"github.com/decred/dcrlnd/lnrpc/walletrpc"
	"github.com/decred/dcrlnd/lnrpc/wtclientrpc"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/dcrwallet"
	"github.com/decred/dcrlnd/lnwallet/remotedcrwallet"
	"github.com/decred/dcrlnd/monitoring"
	"github.com/decred/dcrlnd/netann"
	"github.com/decred/dcrlnd/peernotifier"
	"github.com/decred/dcrlnd/routing"
	"github.com/decred/dcrlnd/signal"
	"github.com/decred/dcrlnd/sweep"
	"github.com/decred/dcrlnd/watchtower"
	"github.com/decred/dcrlnd/watchtower/wtclient"
	sphinx "github.com/decred/lightning-onion/v2"
	"github.com/decred/slog"
	"github.com/jrick/logrotate/rotator"
	"google.golang.org/grpc"
)

// Loggers per subsystem.  A single backend logger is created and all subsystem
// loggers created from it will write to the backend.  When adding new
// subsystems, add the subsystem logger variable here and to the
// subsystemLoggers map.
//
// Loggers can not be used before the log rotator has been initialized with a
// log file.  This must be performed early during application startup by
// calling initLogRotator.
var (
	logWriter = &build.LogWriter{}

	// backendLog is the logging backend used to create all subsystem
	// loggers.  The backend must not be used before the log rotator has
	// been initialized, or data races and/or nil pointer dereferences will
	// occur.
	backendLog = slog.NewBackend(logWriter)

	// logRotator is one of the logging outputs.  It should be closed on
	// application shutdown.
	logRotator *rotator.Rotator

	ltndLog = build.NewSubLogger("LTND", backendLog.Logger)
	lnwlLog = build.NewSubLogger("LNWL", backendLog.Logger)
	kchnLog = build.NewSubLogger("KCHN", backendLog.Logger)
	dcrwLog = build.NewSubLogger("DCRW", backendLog.Logger)
	peerLog = build.NewSubLogger("PEER", backendLog.Logger)
	discLog = build.NewSubLogger("DISC", backendLog.Logger)
	rpcsLog = build.NewSubLogger("RPCS", backendLog.Logger)
	srvrLog = build.NewSubLogger("SRVR", backendLog.Logger)
	ntfnLog = build.NewSubLogger("NTFN", backendLog.Logger)
	chdbLog = build.NewSubLogger("CHDB", backendLog.Logger)
	fndgLog = build.NewSubLogger("FNDG", backendLog.Logger)
	hswcLog = build.NewSubLogger("HSWC", backendLog.Logger)
	utxnLog = build.NewSubLogger("UTXN", backendLog.Logger)
	brarLog = build.NewSubLogger("BRAR", backendLog.Logger)
	cmgrLog = build.NewSubLogger("CMGR", backendLog.Logger)
	crtrLog = build.NewSubLogger("CRTR", backendLog.Logger)
	atplLog = build.NewSubLogger("ATPL", backendLog.Logger)
	cnctLog = build.NewSubLogger("CNCT", backendLog.Logger)
	sphxLog = build.NewSubLogger("SPHX", backendLog.Logger)
	swprLog = build.NewSubLogger("SWPR", backendLog.Logger)
	sgnrLog = build.NewSubLogger("SGNR", backendLog.Logger)
	wlktLog = build.NewSubLogger("WLKT", backendLog.Logger)
	arpcLog = build.NewSubLogger("ARPC", backendLog.Logger)
	invcLog = build.NewSubLogger("INVC", backendLog.Logger)
	nannLog = build.NewSubLogger("NANN", backendLog.Logger)
	wtwrLog = build.NewSubLogger("WTWR", backendLog.Logger)
	ntfrLog = build.NewSubLogger("NTFR", backendLog.Logger)
	irpcLog = build.NewSubLogger("IRPC", backendLog.Logger)
	chnfLog = build.NewSubLogger("CHNF", backendLog.Logger)
	chbuLog = build.NewSubLogger("CHBU", backendLog.Logger)
	promLog = build.NewSubLogger("PROM", backendLog.Logger)
	wtclLog = build.NewSubLogger("WTCL", backendLog.Logger)
	prnfLog = build.NewSubLogger("PRNF", backendLog.Logger)
)

// Initialize package-global logger variables.
func init() {
	lnwallet.UseLogger(lnwlLog)
	dcrwallet.UseLogger(dcrwLog)
	remotedcrwallet.UseLogger(dcrwLog)
	keychain.UseLogger(kchnLog)
	discovery.UseLogger(discLog)
	chainntnfs.UseLogger(ntfnLog)
	channeldb.UseLogger(chdbLog)
	htlcswitch.UseLogger(hswcLog)
	connmgr.UseLogger(cmgrLog)
	routing.UseLogger(crtrLog)
	autopilot.UseLogger(atplLog)
	contractcourt.UseLogger(cnctLog)
	sphinx.UseLogger(sphxLog)
	signal.UseLogger(ltndLog)
	sweep.UseLogger(swprLog)
	signrpc.UseLogger(sgnrLog)
	walletrpc.UseLogger(wlktLog)
	autopilotrpc.UseLogger(arpcLog)
	invoices.UseLogger(invcLog)
	netann.UseLogger(nannLog)
	watchtower.UseLogger(wtwrLog)
	chainrpc.UseLogger(ntfrLog)
	invoicesrpc.UseLogger(irpcLog)
	channelnotifier.UseLogger(chnfLog)
	chanbackup.UseLogger(chbuLog)
	monitoring.UseLogger(promLog)
	wtclient.UseLogger(wtclLog)
	peernotifier.UseLogger(prnfLog)

	addSubLogger(routerrpc.Subsystem, routerrpc.UseLogger)
	addSubLogger(wtclientrpc.Subsystem, wtclientrpc.UseLogger)
}

// addSubLogger is a helper method to conveniently register the logger of a sub
// system.
func addSubLogger(subsystem string, useLogger func(slog.Logger)) {
	logger := build.NewSubLogger(subsystem, backendLog.Logger)
	useLogger(logger)
	subsystemLoggers[subsystem] = logger
}

// subsystemLoggers maps each subsystem identifier to its associated logger.
var subsystemLoggers = map[string]slog.Logger{
	"LTND": ltndLog,
	"LNWL": lnwlLog,
	"KCHN": kchnLog,
	"DCRW": dcrwLog,
	"PEER": peerLog,
	"DISC": discLog,
	"RPCS": rpcsLog,
	"SRVR": srvrLog,
	"NTFN": ntfnLog,
	"CHDB": chdbLog,
	"FNDG": fndgLog,
	"HSWC": hswcLog,
	"UTXN": utxnLog,
	"BRAR": brarLog,
	"CMGR": cmgrLog,
	"CRTR": crtrLog,
	"ATPL": atplLog,
	"CNCT": cnctLog,
	"SPHX": sphxLog,
	"SWPR": swprLog,
	"SGNR": sgnrLog,
	"WLKT": wlktLog,
	"ARPC": arpcLog,
	"INVC": invcLog,
	"NANN": nannLog,
	"WTWR": wtwrLog,
	"NTFR": ntfrLog,
	"IRPC": irpcLog,
	"CHNF": chnfLog,
	"CHBU": chbuLog,
	"PROM": promLog,
	"WTCL": wtclLog,
	"PRNF": prnfLog,
}

// initLogRotator initializes the logging rotator to write logs to logFile and
// create roll files in the same directory.  It must be called before the
// package-global log rotator variables are used.
func initLogRotator(logFile string, maxLogFileSize, maxLogFiles int) {
	logDir, _ := filepath.Split(logFile)
	err := os.MkdirAll(logDir, 0700)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create log directory: %v\n", err)
		os.Exit(1)
	}
	r, err := rotator.New(logFile, int64(maxLogFileSize*1024), false, maxLogFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create file rotator: %v\n", err)
		os.Exit(1)
	}

	pr, pw := io.Pipe()
	go r.Run(pr)

	logWriter.RotatorPipe = pw
	logRotator = r
}

// setLogLevel sets the logging level for provided subsystem.  Invalid
// subsystems are ignored.  Uninitialized subsystems are dynamically created as
// needed.
func setLogLevel(subsystemID string, logLevel string) {
	// Ignore invalid subsystems.
	logger, ok := subsystemLoggers[subsystemID]
	if !ok {
		return
	}

	// Defaults to info if the log level is invalid.
	level, _ := slog.LevelFromString(logLevel)
	logger.SetLevel(level)
}

// setLogLevels sets the log level for all subsystem loggers to the passed
// level. It also dynamically creates the subsystem loggers as needed, so it
// can be used to initialize the logging system.
func setLogLevels(logLevel string) {
	// Configure all sub-systems with the new logging level.  Dynamically
	// create loggers as needed.
	for subsystemID := range subsystemLoggers {
		setLogLevel(subsystemID, logLevel)
	}
}

// logClosure is used to provide a closure over expensive logging operations so
// don't have to be performed when the logging level doesn't warrant it.
type logClosure func() string

// String invokes the underlying function and returns the result.
func (c logClosure) String() string {
	return c()
}

// newLogClosure returns a new closure over a function that returns a string
// which itself provides a Stringer interface so that it can be used with the
// logging system.
func newLogClosure(c func() string) logClosure {
	return logClosure(c)
}

// errorLogUnaryServerInterceptor is a simple UnaryServerInterceptor that will
// automatically log any errors that occur when serving a client's unary
// request.
func errorLogUnaryServerInterceptor(logger slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		resp, err := handler(ctx, req)
		if err != nil {
			// TODO(roasbeef): also log request details?
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return resp, err
	}
}

// errorLogStreamServerInterceptor is a simple StreamServerInterceptor that
// will log any errors that occur while processing a client or server streaming
// RPC.
func errorLogStreamServerInterceptor(logger slog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		err := handler(srv, ss)
		if err != nil {
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return err
	}
}
