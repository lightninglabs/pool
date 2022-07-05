// As this file is very similar in every package, ignore the linter here.
// nolint:dupl,interfacer
package pool

import (
	"context"

	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const Subsystem = "POOL"

var (
	logWriter = build.NewRotatingLogWriter()
	log       = build.NewSubLogger(Subsystem, nil)
	rpcLog    = build.NewSubLogger("RPCS", nil)
	sdcrLog   = build.NewSubLogger("SDCR", nil)
)

// SetupLoggers initializes all package-global logger variables.
func SetupLoggers(root *build.RotatingLogWriter, intercept signal.Interceptor) {
	genLogger := genSubLogger(root, intercept)

	logWriter = root
	log = build.NewSubLogger(Subsystem, genLogger)
	rpcLog = build.NewSubLogger("RPCS", genLogger)
	sdcrLog = build.NewSubLogger("SDCR", genLogger)

	lnd.SetSubLogger(root, Subsystem, log)
	lnd.SetSubLogger(root, "RPCS", rpcLog)
	lnd.SetSubLogger(root, "SDCR", sdcrLog)
	lnd.AddSubLogger(root, funding.Subsystem, intercept, funding.UseLogger)
	lnd.AddSubLogger(
		root, auctioneer.Subsystem, intercept, auctioneer.UseLogger,
	)
	lnd.AddSubLogger(root, order.Subsystem, intercept, order.UseLogger)
	lnd.AddSubLogger(root, "LNDC", intercept, lndclient.UseLogger)
	lnd.AddSubLogger(root, "SGNL", intercept, signal.UseLogger)
	lnd.AddSubLogger(root, account.Subsystem, intercept, account.UseLogger)
	lnd.AddSubLogger(root, lsat.Subsystem, intercept, lsat.UseLogger)
	lnd.AddSubLogger(
		root, clientdb.Subsystem, intercept, clientdb.UseLogger,
	)
	lnd.AddSubLogger(
		root, poolscript.Subsystem, intercept, poolscript.UseLogger,
	)
}

// genSubLogger creates a logger for a subsystem. We provide an instance of
// a signal.Interceptor to be able to shutdown in the case of a critical error.
func genSubLogger(root *build.RotatingLogWriter,
	interceptor signal.Interceptor) func(string) btclog.Logger {

	// Create a shutdown function which will request shutdown from our
	// interceptor if it is listening.
	shutdown := func() {
		if !interceptor.Listening() {
			return
		}

		interceptor.RequestShutdown()
	}

	// Return a function which will create a sublogger from our root
	// logger without shutdown fn.
	return func(tag string) btclog.Logger {
		return root.GenSubLogger(tag, shutdown)
	}
}

// errorLogUnaryServerInterceptor is a simple UnaryServerInterceptor that will
// automatically log any errors that occur when serving a client's unary
// request.
func errorLogUnaryServerInterceptor(logger btclog.Logger) grpc.UnaryServerInterceptor {
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

// errorLogUnaryClientInterceptor is a simple UnaryClientInterceptor that will
// automatically log any errors that occur when executing a unary request to
// the server.
func errorLogUnaryClientInterceptor(logger btclog.Logger) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			logger.Errorf("[%v]: %v", method, err)
		}

		return err
	}
}

// errorLogStreamServerInterceptor is a simple StreamServerInterceptor that
// will log any errors that occur while processing a client or server streaming
// RPC.
func errorLogStreamServerInterceptor(logger btclog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		err := handler(srv, ss)
		if err != nil {
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return err
	}
}

// errorLoggingClientStream wraps around the default client stream to long any errors
// that happen when we attempt to send or receive messages on the main stream.
type errorLoggingClientStream struct {
	grpc.ClientStream

	methodName string

	logger btclog.Logger
}

// RecvMsg attempts to recv a message, but logs an error if it occurs.
func (e *errorLoggingClientStream) RecvMsg(m interface{}) error {
	err := e.ClientStream.RecvMsg(m)
	s, ok := status.FromError(err)
	isCtxCanceledErr := ok && s.Code() == codes.Canceled
	if err != nil && !isCtxCanceledErr {
		e.logger.Errorf("[%v]: %v", e.methodName, err)
	}

	return err
}

// SendMsg attempts to send a message, but logs an error if it occurs.
func (e *errorLoggingClientStream) SendMsg(m interface{}) error {
	err := e.ClientStream.SendMsg(m)
	if err != nil {
		e.logger.Errorf("[%v]: %v", e.methodName, err)
	}

	return err
}

// errorLogStreamClientInterceptor is a simple StreamClientInterceptor that
// will log any errors that occur while processing the messages for a server's
// streaming RPC.
func errorLogStreamClientInterceptor(
	logger btclog.Logger) grpc.StreamClientInterceptor {

	return func(ctx context.Context, desc *grpc.StreamDesc,
		cc *grpc.ClientConn, method string, streamer grpc.Streamer,
		opts ...grpc.CallOption) (grpc.ClientStream, error) {

		mainStream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			logger.Errorf("[%v]: %v", method, err)
			return nil, err
		}

		return &errorLoggingClientStream{
			ClientStream: mainStream,
			logger:       logger,
			methodName:   method,
		}, nil
	}
}
